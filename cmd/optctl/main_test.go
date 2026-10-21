package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/model"
)

// initOptctl 建一个临时库并装载配置，模拟 optctl 的真实启动前置。
func initOptctl(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "optctl.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	t.Setenv("LOG_LEVEL", "error")
	common.InitEnv()

	t.Cleanup(func() {
		_ = model.CloseDB()
		model.OptionMap = map[string]string{}
	})

	if err := model.InitDB(); err != nil {
		t.Fatalf("model.InitDB 失败: %v", err)
	}
	if err := model.InitOptionMap(); err != nil {
		t.Fatalf("model.InitOptionMap 失败: %v", err)
	}
}

func TestSetThenGet(t *testing.T) {
	initOptctl(t)

	var out bytes.Buffer
	if err := run([]string{"set", "system_setting.status_message", `"工具改的"`}, &out); err != nil {
		t.Fatalf("set 失败: %v", err)
	}

	out.Reset()
	if err := run([]string{"get", "system_setting.status_message"}, &out); err != nil {
		t.Fatalf("get 失败: %v", err)
	}
	if strings.TrimSpace(out.String()) != `"工具改的"` {
		t.Errorf("get 输出 = %q，期望 %q", out.String(), `"工具改的"`)
	}
}

func TestListShowsAllKeys(t *testing.T) {
	initOptctl(t)

	var out bytes.Buffer
	if err := run([]string{"list"}, &out); err != nil {
		t.Fatalf("list 失败: %v", err)
	}

	for _, key := range []string{
		"system_setting.log_level",
		"system_setting.status_message",
		"system_setting.enable_access_log",
		"system_setting.slow_request_threshold_seconds",
	} {
		if !strings.Contains(out.String(), key) {
			t.Errorf("list 输出缺少 %q：\n%s", key, out.String())
		}
	}
}

// 非法输入必须报错并且**不落库**：optctl 是唯一能从外部改配置的工具，
// 它的错误直接变成线上配置。
func TestRejectsBadInput(t *testing.T) {
	initOptctl(t)

	cases := []struct {
		name string
		args []string
	}{
		{"未知 key", []string{"set", "system_setting.nope", `"x"`}},
		{"未知模块", []string{"set", "nope.whatever", `"x"`}},
		{"类型不符", []string{"set", "system_setting.enable_access_log", `"yes"`}},
		{"值不是合法 JSON", []string{"set", "system_setting.status_message", `hello`}},
		{"set 缺参数", []string{"set", "system_setting.status_message"}},
		{"get 缺参数", []string{"get"}},
		{"未知子命令", []string{"delete", "system_setting.status_message"}},
		{"无子命令", []string{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(c.args, &out); err == nil {
				t.Fatalf("run(%v) 应该报错", c.args)
			}
		})
	}

	var count int64
	if err := model.DB.Model(&model.Option{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("失败的命令不该写入任何行，实际有 %d 行", count)
	}
}

func TestGetUnknownKeyFails(t *testing.T) {
	initOptctl(t)

	var out bytes.Buffer
	if err := run([]string{"get", "system_setting.nope"}, &out); err == nil {
		t.Error("读取未注册的 key 应该报错")
	}
}

func TestAddFixtures(t *testing.T) {
	initOptctl(t)

	var out bytes.Buffer
	if err := run([]string{"user", "add", "alice", "default", "1000000"}, &out); err != nil {
		t.Fatalf("user add: %v", err)
	}
	if err := run([]string{"token", "add", "sk-local", "1"}, &out); err != nil {
		t.Fatalf("token add: %v", err)
	}
	if err := run([]string{"channel", "add", "1", "sk-upstream", "http://127.0.0.1:8080", "gpt-4o,gpt-4o-mini"}, &out); err != nil {
		t.Fatalf("channel add: %v", err)
	}

	var users []model.User
	if err := model.DB.Find(&users).Error; err != nil || len(users) != 1 {
		t.Fatalf("用户数 = %d err=%v", len(users), err)
	}
	if users[0].Username != "alice" || users[0].GroupName != "default" || users[0].Quota != 1000000 {
		t.Errorf("用户字段不对: %+v", users[0])
	}

	var tokens []model.Token
	if err := model.DB.Find(&tokens).Error; err != nil || len(tokens) != 1 {
		t.Fatalf("令牌数 = %d err=%v", len(tokens), err)
	}
	if tokens[0].Key != "sk-local" || !tokens[0].UnlimitedQuota || tokens[0].ExpiredTime != -1 {
		t.Errorf("令牌字段不对: %+v", tokens[0])
	}

	var channels []model.Channel
	if err := model.DB.Find(&channels).Error; err != nil || len(channels) != 1 {
		t.Fatalf("渠道数 = %d err=%v", len(channels), err)
	}
	if channels[0].Type != 1 || channels[0].Status != 1 || channels[0].GroupName != "default" {
		t.Errorf("渠道字段不对: %+v", channels[0])
	}

	if err := run([]string{"channel", "add", "9", "k", "http://x", "m"}, &out); err == nil {
		t.Error("未知渠道类型应报错")
	}
	if err := run([]string{"token", "add", "sk-x", "999"}, &out); err == nil {
		t.Error("不存在的 user-id 应报错")
	}
}
