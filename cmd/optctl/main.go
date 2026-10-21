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
	"strconv"
	"strings"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/constant"
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

	case "user":
		return runUser(args[1:], out)

	case "token":
		return runToken(args[1:], out)

	case "channel":
		return runChannel(args[1:], out)

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

// ---------- 夹具命令 ----------
//
// M1 还没有管理面（M3），但要让"端到端跑一次转发"可操作，就必须有一个
// 从进程外创建用户/令牌/渠道的手段。这三个命令只做最少的字段填充，
// 不承担校验职责——字段合法性与运行时的关系由 model 层保证。

func runUser(args []string, out io.Writer) error {
	if len(args) < 2 || args[0] != "add" || args[1] == "" {
		return fmt.Errorf("用法: optctl user add <username> [<group>] [<quota>]\n%s", usage)
	}
	group, quota := "default", int64(0)
	if len(args) >= 3 && strings.TrimSpace(args[2]) != "" {
		group = strings.TrimSpace(args[2])
	}
	if len(args) >= 4 {
		n, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return fmt.Errorf("quota 必须是整数: %w", err)
		}
		quota = n
	}

	u := model.User{
		Username:  args[1],
		Role:      constant.RoleUser,
		Status:    constant.UserStatusEnabled,
		GroupName: group,
		Quota:     quota,
	}
	if err := model.DB.Create(&u).Error; err != nil {
		return fmt.Errorf("创建用户失败: %w", err)
	}
	fmt.Fprintf(out, "user id=%d username=%s group=%s quota=%d\n", u.ID, u.Username, u.GroupName, u.Quota)
	return nil
}

func runToken(args []string, out io.Writer) error {
	if len(args) < 3 || args[0] != "add" || args[1] == "" {
		return fmt.Errorf("用法: optctl token add <key> <user-id> [<remain-quota>]\n%s", usage)
	}
	userID, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("user-id 必须是整数: %w", err)
	}
	user, err := model.GetUserByID(userID)
	if err != nil {
		return fmt.Errorf("用户 %d 不存在: %w", userID, err)
	}

	// 缺省/负数=无限，零或正数=限额。
	unlimited, remainQuota := true, int64(0)
	if len(args) >= 4 {
		n, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return fmt.Errorf("remain-quota 必须是整数: %w", err)
		}
		if n >= 0 {
			unlimited, remainQuota = false, n
		}
	}

	tok := model.Token{
		Key:            args[1],
		UserID:         userID,
		Status:         constant.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: unlimited,
		RemainQuota:    remainQuota,
		GroupName:      user.GroupName,
	}
	if err := model.DB.Create(&tok).Error; err != nil {
		return fmt.Errorf("创建令牌失败: %w", err)
	}
	fmt.Fprintf(out, "token id=%d key=%s user_id=%d group=%s remain_quota=%d unlimited=%v\n",
		tok.ID, tok.Key, tok.UserID, tok.GroupName, tok.RemainQuota, tok.UnlimitedQuota)
	return nil
}

func runChannel(args []string, out io.Writer) error {
	if len(args) < 5 || args[0] != "add" {
		return fmt.Errorf("用法: optctl channel add <type> <key> <base-url> <models> [<group>] [<priority>] [<weight>]\n%s", usage)
	}
	apiType, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("type 必须是整数: %w", err)
	}
	if apiType != constant.APITypeOpenAI {
		return fmt.Errorf("M1 只支持 type=%d（OpenAI 兼容），收到 %d", constant.APITypeOpenAI, apiType)
	}

	group, priority, weight := "default", int64(0), uint(1)
	if len(args) >= 6 && strings.TrimSpace(args[5]) != "" {
		group = strings.TrimSpace(args[5])
	}
	if len(args) >= 7 {
		if priority, err = strconv.ParseInt(args[6], 10, 64); err != nil {
			return fmt.Errorf("priority 必须是整数: %w", err)
		}
	}
	if len(args) >= 8 {
		w, err := strconv.ParseUint(args[7], 10, 32)
		if err != nil {
			return fmt.Errorf("weight 必须是非负整数: %w", err)
		}
		weight = uint(w)
	}

	ch := model.Channel{
		Type:      apiType,
		Key:       args[2],
		BaseURL:   strings.TrimRight(args[3], "/"),
		Models:    args[4],
		GroupName: group,
		Priority:  priority,
		Weight:    weight,
		Status:    constant.ChannelStatusEnabled,
	}
	if err := model.DB.Create(&ch).Error; err != nil {
		return fmt.Errorf("创建渠道失败: %w", err)
	}
	fmt.Fprintf(out, "channel id=%d type=%d models=%s group=%s priority=%d weight=%d\n",
		ch.ID, ch.Type, ch.Models, ch.GroupName, ch.Priority, ch.Weight)
	return nil
}

// usage 是变量而非常量：strings.TrimSpace 不是常量表达式。
var usage = strings.TrimSpace(`
用法:
  optctl get  <key>
  optctl set  <key> <json-value>
  optctl list
  optctl user    add <username> [<group>] [<quota>]
  optctl token   add <key> <user-id> [<remain-quota>]  # 缺省/负数=无限，非负=限额
  optctl channel add <type> <key> <base-url> <models> [<group>] [<priority>] [<weight>]

值必须是 JSON 编码，字符串要带引号：
  optctl set system_setting.status_message '"你好"'
  optctl set system_setting.enable_access_log false
`)
