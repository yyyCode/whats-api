// Command optctl 直接读写 Option 表，绕过 HTTP。
//
// 为什么 M0 需要它：管理面的写接口是 M3 的事，而验收标准③要求验证
// "多实例改配置后能在 SYNC_FREQUENCY 内收敛"。要证这件事，就必须有一个
// **从进程外部**改配置的手段——否则测试改的是同一个进程的内存，等于没测同步。
//
// 它只做一层薄壳：解析参数 → 调 model.UpdateOption / OptionMap。
// 所有校验都在 model 层，因此它的行为与运行时完全一致（不会造出运行时拒绝的配置）。
//
// 用法：
//
//	optctl get  <key>
//	optctl set  <key> <json-value>
//	optctl list
//
// 值必须是 JSON 编码：字符串要带引号 —— optctl set system_setting.status_message '"你好"'
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
)

func main() {
	if err := setupOptctl(); err != nil {
		fmt.Fprintf(os.Stderr, "optctl 初始化失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = model.CloseDB() }()

	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "optctl: %v\n", err)
		os.Exit(1)
	}
}

// setupOptctl 走与运行时相同的初始化路径（环境变量 → 主库 → 配置装载），
// 保证工具看到的配置与进程看到的一致。
//
// 注意：测试文件里另有一个 initOptctl(t *testing.T) 辅助函数（计划原文如此），
// 二者同包同名会冲突，故此处用 setupOptctl 命名。
func setupOptctl() error {
	common.InitEnv()
	logger.SetupLogger(common.LogLevel, false)

	if err := model.InitDB(); err != nil {
		return err
	}
	return model.InitOptionMap()
}

// run 是命令实现，与 main 分开是为了可测试：测试直接调 run，不必 exec 二进制。
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("缺少子命令\n%s", usage)
	}

	switch args[0] {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("get 需要 <key> 参数\n%s", usage)
		}
		return runGet(args[1], out)

	case "set":
		if len(args) < 3 {
			return fmt.Errorf("set 需要 <key> <json-value> 两个参数\n%s", usage)
		}
		return runSet(args[1], args[2])

	case "list":
		return runList(out)

	default:
		return fmt.Errorf("未知子命令 %q\n%s", args[0], usage)
	}
}

func runGet(key string, out io.Writer) error {
	value, ok := model.OptionMap[key]
	if !ok {
		return fmt.Errorf("未注册的配置项 %q", key)
	}
	fmt.Fprintln(out, value)
	return nil
}

func runSet(key, value string) error {
	if err := model.UpdateOption(key, value); err != nil {
		return err
	}
	// 回显写入后的值：让人确认"写进去的是什么"，而不是相信自己记得参数的引号。
	fmt.Printf("%s = %s\n", key, model.OptionMap[key])
	return nil
}

func runList(out io.Writer) error {
	keys := make([]string, 0, len(model.OptionMap))
	for key := range model.OptionMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(out, "%s = %s\n", key, model.OptionMap[key])
	}
	return nil
}

// usage 是变量而非常量：strings.TrimSpace 不是常量表达式。
var usage = strings.TrimSpace(`
用法:
  optctl get  <key>
  optctl set  <key> <json-value>
  optctl list

值必须是 JSON 编码，字符串要带引号：
  optctl set system_setting.status_message '"你好"'
  optctl set system_setting.enable_access_log false
`)
