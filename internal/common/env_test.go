package common

import (
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
	InitEnv()

	if SyncFrequency != MinSyncFrequencySeconds || len(EnvWarnings) != 0 {
		t.Errorf("恰好等于下限不应告警：sync=%d warnings=%v", SyncFrequency, EnvWarnings)
	}
}

// 非法数值不致命：回退默认值继续启动，而不是启动不了。
func TestInitEnvFallsBackOnInvalidNumbers(t *testing.T) {
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
