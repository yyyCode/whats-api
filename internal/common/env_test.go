package common

import (
	"strconv"
	"strings"
	"testing"
)

// 清空所有本包读取的环境变量——"不配任何环境变量"是第一条验收标准的前提。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORT", "GIN_MODE", "CORS_ALLOW_ORIGINS", "SQL_DSN", "LOG_SQL_DSN",
		"REDIS_CONN_STRING", "NODE_TYPE", "NODE_NAME", "SYNC_FREQUENCY",
		"SHUTDOWN_TIMEOUT_SECONDS", "LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
}

func TestInitEnvDefaultsWithNoEnv(t *testing.T) {
	clearEnv(t)
	InitEnv()

	if Port != DefaultPort {
		t.Errorf("Port = %d，期望默认值 %d", Port, DefaultPort)
	}
	if GinMode != "release" {
		t.Errorf("GinMode = %q，期望 release", GinMode)
	}
	if SQLDSN != "" {
		t.Errorf("SQLDSN = %q，期望空（空 = SQLite 默认文件）", SQLDSN)
	}
	if !IsMasterNode || NodeType != DefaultNodeType {
		t.Errorf("默认应为 master 节点，得到 NodeType=%q IsMasterNode=%v", NodeType, IsMasterNode)
	}
	if SyncFrequency != DefaultSyncFrequencySeconds {
		t.Errorf("SyncFrequency = %d，期望 %d", SyncFrequency, DefaultSyncFrequencySeconds)
	}
	if ShutdownTimeout != DefaultShutdownTimeoutSeconds {
		t.Errorf("ShutdownTimeout = %d，期望 %d", ShutdownTimeout, DefaultShutdownTimeoutSeconds)
	}
	if LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q，期望 %q", LogLevel, DefaultLogLevel)
	}
	if len(CORSAllowOrigins) != 1 || CORSAllowOrigins[0] != "*" {
		t.Errorf("CORSAllowOrigins = %v，期望 [*]", CORSAllowOrigins)
	}
	if len(EnvWarnings) != 0 {
		t.Errorf("默认配置不应产生告警，得到 %v", EnvWarnings)
	}
}

func TestInitEnvReadsOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "8080")
	t.Setenv("SQL_DSN", "local:/tmp/whats.db")
	t.Setenv("LOG_SQL_DSN", "postgres://localhost/logs")
	t.Setenv("REDIS_CONN_STRING", "127.0.0.1:6379")
	t.Setenv("NODE_TYPE", "slave")
	t.Setenv("NODE_NAME", "node-b")
	t.Setenv("SYNC_FREQUENCY", "15")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "30")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("CORS_ALLOW_ORIGINS", "https://a.example.com, https://b.example.com ")
	InitEnv()

	if Port != 8080 {
		t.Errorf("Port = %d，期望 8080", Port)
	}
	if SQLDSN != "local:/tmp/whats.db" {
		t.Errorf("SQLDSN = %q", SQLDSN)
	}
	if LogSQLDSN != "postgres://localhost/logs" {
		t.Errorf("LogSQLDSN = %q", LogSQLDSN)
	}
	if RedisConnString != "127.0.0.1:6379" {
		t.Errorf("RedisConnString = %q", RedisConnString)
	}
	if IsMasterNode {
		t.Error("NODE_TYPE=slave 时 IsMasterNode 必须为 false")
	}
	if NodeName != "node-b" {
		t.Errorf("NodeName = %q", NodeName)
	}
	if SyncFrequency != 15 || ShutdownTimeout != 30 || LogLevel != "debug" {
		t.Errorf("秒级配置读取错误：sync=%d shutdown=%d level=%q", SyncFrequency, ShutdownTimeout, LogLevel)
	}
	want := []string{"https://a.example.com", "https://b.example.com"}
	if len(CORSAllowOrigins) != len(want) {
		t.Fatalf("CORSAllowOrigins = %v，期望 %v", CORSAllowOrigins, want)
	}
	for i := range want {
		if CORSAllowOrigins[i] != want[i] {
			t.Errorf("CORSAllowOrigins[%d] = %q，期望 %q（应去掉空格）", i, CORSAllowOrigins[i], want[i])
		}
	}
}

// 同步频率太低会把业务库当轮询靶子，所以低于下限要抬回来并告警。
func TestInitEnvClampsSyncFrequencyAndWarns(t *testing.T) {
	clearEnv(t)
	t.Setenv("SYNC_FREQUENCY", "1")
	InitEnv()

	if SyncFrequency != MinSyncFrequencySeconds {
		t.Errorf("SyncFrequency = %d，期望被抬到下限 %d", SyncFrequency, MinSyncFrequencySeconds)
	}
	if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "SYNC_FREQUENCY") {
		t.Errorf("应产生一条 SYNC_FREQUENCY 告警，得到 %v", EnvWarnings)
	}
}

func TestInitEnvDoesNotWarnAtExactMinimum(t *testing.T) {
	clearEnv(t)
	t.Setenv("SYNC_FREQUENCY", "5")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "5")
	InitEnv()

	if SyncFrequency != MinSyncFrequencySeconds || len(EnvWarnings) != 0 {
		t.Errorf("恰好等于下限不应告警：sync=%d warnings=%v", SyncFrequency, EnvWarnings)
	}
}

// 优雅退出的超时会被当成 deadline 用，0 或负数等于"立刻取消"，
// 等于把优雅退出变成强杀；太大则让容器停不下来。下限必须被抬回来。
func TestInitEnvClampsShutdownTimeout(t *testing.T) {
	for _, in := range []string{"0", "-1"} {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", in)
			InitEnv()

			if ShutdownTimeout != MinShutdownTimeoutSeconds {
				t.Errorf("SHUTDOWN_TIMEOUT_SECONDS=%s 应被抬到下限 %d，得到 %d",
					in, MinShutdownTimeoutSeconds, ShutdownTimeout)
			}
			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "SHUTDOWN_TIMEOUT_SECONDS") {
				t.Errorf("应产生一条 SHUTDOWN_TIMEOUT_SECONDS 告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 端口超出 1-65535 是配置错误：回退默认值并告警，而不是把 0 端口交给监听。
func TestInitEnvRejectsOutOfRangePort(t *testing.T) {
	for _, in := range []string{"0", "-1", "65536", "99999"} {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PORT", in)
			InitEnv()

			if Port != DefaultPort {
				t.Errorf("PORT=%s 应回退 %d，得到 %d", in, DefaultPort, Port)
			}
			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "PORT") {
				t.Errorf("应产生一条 PORT 告警，得到 %v", EnvWarnings)
			}
		})
	}

	// 边界值合法，不该被误伤。
	for _, in := range []string{"1", "65535"} {
		t.Run("边界 "+in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PORT", in)
			InitEnv()

			if port, _ := strconv.Atoi(in); Port != port {
				t.Errorf("PORT=%s 是合法端口，不该被改，得到 %d", in, Port)
			}
			if len(EnvWarnings) != 0 {
				t.Errorf("合法端口不该告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 大小写不该决定主从：NODE_TYPE=Slave 若按原样比较，从节点会被判成主节点，
// 从而去跑迁移——这是"多节点同时建表"最直接的来源。
func TestInitEnvNodeTypeIsCaseInsensitive(t *testing.T) {
	cases := []struct {
		in         string
		wantMaster bool
	}{
		{"master", true},
		{"MASTER", true},
		{"Master", true},
		{"slave", false},
		{"SLAVE", false},
		{"Slave", false},
		{"  slave  ", false}, // envStr 会去空白
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NODE_TYPE", c.in)
			InitEnv()

			if IsMasterNode != c.wantMaster {
				t.Errorf("NODE_TYPE=%q 时 IsMasterNode = %v，期望 %v", c.in, IsMasterNode, c.wantMaster)
			}
			if len(EnvWarnings) != 0 {
				t.Errorf("大小写差异不该告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 认不出来的取值按默认处理，但必须告警——否则运维写错一个词，
// 主从角色就静默变了，而日志里什么都没有。
func TestInitEnvWarnsOnUnknownNodeType(t *testing.T) {
	clearEnv(t)
	t.Setenv("NODE_TYPE", "primary")
	InitEnv()

	if NodeType != DefaultNodeType || !IsMasterNode {
		t.Errorf("NODE_TYPE=primary 应按默认 %q 处理，得到 %q", DefaultNodeType, NodeType)
	}
	if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "NODE_TYPE") {
		t.Errorf("应产生一条 NODE_TYPE 告警，得到 %v", EnvWarnings)
	}
}

// CORS_ALLOW_ORIGINS 配了但一个有效条目都没有（如 ","）：必须按"谁都不允许"
// 处理并告警，绝不能回落到默认值 "*"——那是允许所有来源，
// 会把一次本想收紧的配置变成完全放开。
func TestInitEnvTreatsEmptyCORSListAsDenyAll(t *testing.T) {
	for _, in := range []string{",", " , ", ",,"} {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("CORS_ALLOW_ORIGINS", in)
			InitEnv()

			if len(CORSAllowOrigins) != 0 {
				t.Errorf("CORS_ALLOW_ORIGINS=%q 应得到空列表，得到 %v", in, CORSAllowOrigins)
			}
			for _, o := range CORSAllowOrigins {
				if o == "*" {
					t.Fatalf("CORS_ALLOW_ORIGINS=%q 回落到通配符 %q，等于完全放开", in, o)
				}
			}
			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "CORS_ALLOW_ORIGINS") {
				t.Errorf("应产生一条 CORS_ALLOW_ORIGINS 告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 非法数值不致命：回退默认值继续启动，而不是启动不了。
// 但回退必须留痕——否则 PORT=abc 会安安静静跑在默认端口上，
// 而运维以为服务听在他们配的端口上。
func TestInitEnvFallsBackOnInvalidNumbers(t *testing.T) {
	cases := []struct {
		key string
		val string
	}{
		{"PORT", "not-a-port"},
		{"PORT", "3000.5"},
		{"SHUTDOWN_TIMEOUT_SECONDS", "abc"},
		{"SYNC_FREQUENCY", "60秒"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(c.key, c.val)
			InitEnv()

			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], c.key) {
				t.Errorf("%s=%q 回退默认值时应留下一条告警，得到 %v", c.key, c.val, EnvWarnings)
			}
		})
	}

	clearEnv(t)
	t.Setenv("PORT", "not-a-port")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "abc")
	InitEnv()
	if Port != DefaultPort {
		t.Errorf("非法 PORT 应回退 %d，得到 %d", DefaultPort, Port)
	}
	if ShutdownTimeout != DefaultShutdownTimeoutSeconds {
		t.Errorf("非法超时应回退 %d，得到 %d", DefaultShutdownTimeoutSeconds, ShutdownTimeout)
	}
}
