package cachex

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)

// 这组测试针对的是 Counter 最难在单机复现的那条路径：
// **Redis 已启用、但运行中开始报错**。
//
// 为什么不用 mock 而用假服务器：要验证的正是"真驱动在这种情形下确实返回 error"。
// 如果只 mock 一个 error 出来，测的是我们自己的假设，不是 go-redis 的行为。
// 假服务器只认 PING，其余命令一律回错误，于是 InitRedisClient 能连上（所以
// RedisEnabled() 为真），而之后的每个命令都失败——正好是"掉线"的形状。
//
// 这条路径上曾经有过一个很难查的 bug：Incr 在 Redis 报错时回落到内存写入，
// 而 Get 仍去读 Redis 得到 miss，于是刚加上的计数立刻"不存在"，
// 看日志还以为压根没写进去。两边必须对称。

// fakeRedis 是一个只回 PING 的假 Redis：用作"连得上但用不了"的服务器。
type fakeRedis struct {
	ln net.Listener
}

// startFakeRedis 起一个假 Redis 并返回它。调用方负责 Close。
func startFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	f := &fakeRedis{ln: ln}
	t.Cleanup(func() { _ = ln.Close() }) // Accept 随即返回错误，处理协程退出

	go f.serve()
	return f
}

// addr 返回可直接写进 REDIS_CONN_STRING 的 host:port。
func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // 监听器已关闭
		}
		go f.handle(conn)
	}
}

// errReply 是对除 PING 外所有命令的回答。
const errReply = "-ERR 假 Redis 只认 PING\r\n"

func (f *fakeRedis) handle(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	var inTx bool
	var queued int

	for {
		cmd, err := readRESPCommand(br)
		if err != nil {
			return
		}

		switch strings.ToUpper(cmd) {
		case "PING":
			_, _ = io.WriteString(conn, "+PONG\r\n")

		case "MULTI":
			inTx, queued = true, 0
			_, _ = io.WriteString(conn, "+OK\r\n")

		case "EXEC":
			// 真 Redis 把各命令的结果按顺序放进数组，错误也在数组元素里。
			// 照着回，客户端才不会因为"读到的字节数和预期对不上"而丢弃连接
			// ——那会打出 "Conn has unread data" 的噪声，让人以为测试本身有问题。
			_, _ = fmt.Fprintf(conn, "*%d\r\n", queued)
			for i := 0; i < queued; i++ {
				_, _ = io.WriteString(conn, errReply)
			}
			inTx, queued = false, 0

		default:
			if inTx { // 事务中：真 Redis 只回 QUEUED，并不执行
				_, _ = io.WriteString(conn, "+QUEUED\r\n")
				queued++
				continue
			}
			_, _ = io.WriteString(conn, errReply)
		}
	}
}

// readRESPCommand 读一条请求并返回它的子命令名（inline 形式也认）。
func readRESPCommand(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("空请求")
	}

	if !strings.HasPrefix(line, "*") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return "", errors.New("空 inline 命令")
		}
		return fields[0], nil
	}

	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return "", fmt.Errorf("非法的数组头 %q: %w", line, err)
	}
	first := ""
	for i := 0; i < n; i++ {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if !strings.HasPrefix(hdr, "$") {
			return "", fmt.Errorf("非法的批量字符串头 %q", hdr)
		}
		length, err := strconv.Atoi(hdr[1:])
		if err != nil {
			return "", fmt.Errorf("非法的长度 %q: %w", hdr, err)
		}
		buf := make([]byte, length+2) // 尾部 CRLF
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", err
		}
		if i == 0 {
			first = string(buf[:length])
		}
	}
	return first, nil
}

// 把 common 接到假 Redis 上：连得上（所以 RedisEnabled 为真），但命令都会失败。
func useFailingRedis(t *testing.T) {
	t.Helper()

	f := startFakeRedis(t)
	t.Cleanup(func() {
		_ = common.CloseRedis()
		common.RedisConnString = ""
	})

	common.RedisConnString = f.addr() // normalizeRedisURL 会补上 redis://
	common.InitRedisClient()

	if !common.RedisEnabled() {
		t.Fatal("前置条件不成立：假 Redis 应该能让 InitRedisClient 报告已启用")
	}
}

func TestCounterFallsBackToMemoryWhenRedisErrors(t *testing.T) {
	useFailingRedis(t)

	c := NewCounter("test_fallback", time.Minute)

	// Backend() 报告的是**选用**的后端，不含运行期单次调用的成败：
	// 此刻它仍应说 redis，尽管下面的调用都会回落到内存。
	if c.Backend() != "redis" {
		t.Fatalf("Backend() = %q，期望 redis（它反映的是选用结果，不是单次成败）", c.Backend())
	}

	if _, err := c.Incr("k"); err != nil {
		t.Fatalf("Redis 报错时 Incr 必须回落而不是把错误抛出去: %v", err)
	}
	// 这一条才是关键：Incr 写进了内存，Get 就必须从同一个内存里读。
	// 不对称的话刚加上的计数会立刻"不存在"。
	got, ok := c.Get("k")
	if !ok || got != 1 {
		t.Errorf("Redis 报错时 Get 必须回落到同一个内存后端：Get = (%d, %v)，期望 (1, true)", got, ok)
	}

	if _, err := c.Incr("k"); err != nil {
		t.Fatalf("第二次 Incr 失败: %v", err)
	}
	if got, ok := c.Get("k"); !ok || got != 2 {
		t.Errorf("Get = (%d, %v)，期望 (2, true)", got, ok)
	}
}
