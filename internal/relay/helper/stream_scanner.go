package helper

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

const (
	// maxSSELineBytes：单个 SSE 行可以很大（长 token、base64 图片），
	// bufio.Scanner 默认 64KB 上限会在这种请求上直接报 "token too long"。
	maxSSELineBytes = 1 << 20
	// streamChanSize 是读/写两个 goroutine 之间的缓冲。
	// 有缓冲才能在客户端慢一拍时继续读上游，而不是把上游连接也卡住。
	streamChanSize = 64
)

type streamEvent struct {
	data string
	err  error
}

// StreamScannerHandler 把上游 SSE 逐事件转发给客户端。
//
// 为什么读与写必须是两个 goroutine：流式转发同时受三个时钟约束——
// 上游出数据的节奏、客户端读取的节奏、保活时钟。单 goroutine 在
// "上游 30 秒不出数据"时就无法感知客户端已经断开，上游的 token 会白烧。
//
// 返回值约定：
//   - nil：正常收到 [DONE]，或客户端主动断开（不写响应）
//   - 非 nil：上游流异常结束；此时 ResponseWritten 已置位，调用方不会写响应
func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) *types.APIError {
	defer resp.Body.Close()

	setEventStreamHeaders(c)
	c.Writer.WriteHeaderNow()
	c.Writer.Flush()
	// 响应头已发出：从此刻起不能再换渠道，否则客户端会收到两段流。
	info.AddResponseWritten(1)

	pingInterval := info.Stream.PingInterval
	if pingInterval <= 0 {
		pingInterval = relaycommon.DefaultPingInterval
	}
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	events := make(chan streamEvent, streamChanSize)
	stop := make(chan struct{})
	defer close(stop)

	go scanSSE(resp.Body, events, stop)

	sawDone := false
	for {
		select {
		case <-c.Request.Context().Done():
			// 客户端断开：不写任何东西，也不该禁用渠道。
			return nil

		case <-ticker.C:
			if _, err := c.Writer.WriteString(": keep-alive\n\n"); err != nil {
				return nil // 写不进去说明连接已断，等同客户端断开
			}
			c.Writer.Flush()

		case ev, ok := <-events:
			if !ok {
				if sawDone {
					return nil
				}
				return types.NewUpstreamError(http.StatusBadGateway, "上游流在 [DONE] 之前结束")
			}
			if ev.err != nil {
				logger.Warn("读取上游流失败", "channel_id", info.Attempt.ChannelID, "error", ev.err)
				return types.NewUpstreamError(http.StatusBadGateway, "读取上游流失败: "+ev.err.Error())
			}
			if ev.data == "[DONE]" {
				sawDone = true
			} else {
				// 在 handler goroutine 里解析 usage，而不是在 scanSSE goroutine 里：
				// stream 的生命周期归 handler 所有，早退（客户端断开、写失败）后
				// 调用方会立即结算预扣；若扫描 goroutine 还在往 info.Attempt.Usage
				// 写入，就会和结算读 usage 形成数据竞争。这里串行处理，天然无锁。
				// 每一帧都要看：某些兼容网关把累积 usage 放在倒数第二帧；
				// MergeNonZero 保证后到的零值不会抹掉已采集到的非零用量。
				mergeStreamUsage(info, ev.data)
			}
			n, err := c.Writer.WriteString("data: " + ev.data + "\n\n")
			info.AddResponseWritten(n)
			if err != nil {
				return nil
			}
			c.Writer.Flush()
			if sawDone {
				return nil
			}
		}
	}
}

// mergeStreamUsage 尝试从一帧 SSE data 里解析 usage 并合并到本次尝试。
// 大多数帧没有 usage 字段（只有 delta），解析失败是常态，直接忽略。
func mergeStreamUsage(info *relaycommon.RelayInfo, data string) {
	var event struct {
		Usage *types.Usage `json:"usage"`
	}
	if err := common.Unmarshal([]byte(data), &event); err != nil || event.Usage == nil {
		return
	}
	if info.Attempt.Usage == nil {
		info.Attempt.Usage = event.Usage
		return
	}
	info.Attempt.Usage.MergeNonZero(event.Usage)
}

func setEventStreamHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 关掉 Nginx 的响应缓冲，否则事件会被攒着一起发，流式就名存实亡。
	h.Set("X-Accel-Buffering", "no")
}

// scanSSE 逐行读上游，只把 data 行送进 channel。
// OpenAI 的流只需要 data 行：event/id/comment 对我们没有语义。
func scanSSE(r io.Reader, events chan<- streamEvent, stop <-chan struct{}) {
	defer close(events)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxSSELineBytes)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		select {
		case events <- streamEvent{data: data}:
		case <-stop:
			return
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		select {
		case events <- streamEvent{err: err}:
		case <-stop:
		}
	}
}
