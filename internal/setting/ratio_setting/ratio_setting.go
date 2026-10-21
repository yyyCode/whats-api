// Package ratio_setting 是定价倍率配置域。
//
// 倍率表（模型 → 倍率）条目多、调价频繁，如果每个模型都做成一个配置字段，
// 配置注册表会被撑爆；所以整张表以 JSON 字符串存进一个配置项，由 OnRebuild
// 解析成不可变快照。热路径只做一次 atomic 读 + map 查找，不需要锁。
//
// 倍率语义（与 M2 计费公式一致）：
//   - model_ratio 是「美元 / 100 万输入 token」；
//   - completion_ratio 是输出 token 相对输入的倍数，乘在 model_ratio 上；
//   - group_ratio 乘在最终金额上。
package ratio_setting

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/setting/config"
)

// RatioSetting 是定价倍率域的持久化形态。
//
// 三张表用 JSON 字符串而不是逐个模型建字段：表的形状会随供应商变化，
// 配置注册表只支持基础类型，字符串是唯一能容纳任意条目的选择。
type RatioSetting struct {
	// ModelRatios 是 {模型名: 美元/百万输入 token} 的 JSON 表。
	ModelRatios string `json:"model_ratios"`

	// CompletionRatios 是 {模型名: 输出相对输入的倍数} 的 JSON 表。
	CompletionRatios string `json:"completion_ratios"`

	// GroupRatios 是 {分组名: 最终金额倍数} 的 JSON 表；未配置的分组按 1 处理。
	GroupRatios string `json:"group_ratios"`

	// DefaultModelRatio 是模型未出现在 ModelRatios 里时的兜底输入价。
	DefaultModelRatio float64 `json:"default_model_ratio"`

	// DefaultCompletionRatio 是模型未出现在 CompletionRatios 里时的兜底输出倍数。
	DefaultCompletionRatio float64 `json:"default_completion_ratio"`

	// InputPreConsumeMultiplier 是预扣输入费时的安全系数。
	// 输出长度在调用前未知，预扣只覆盖输入；乘一个大于 1 的系数是为了让
	// 结算时的补扣尽量小，但不改变最终金额（结算公式不乘它）。
	InputPreConsumeMultiplier float64 `json:"input_pre_consume_multiplier"`
}

// Default 是本域的默认值。
//
// 默认倍率的单位是「美元 / 100 万 token」，与运营看到的价目表一致；
// 只有管理员显式改过的项才落库，这样将来调默认值能影响所有未修改的实例。
var Default = RatioSetting{
	ModelRatios:               `{"gpt-4o":2.5,"gpt-4o-mini":0.15}`,
	CompletionRatios:          `{"gpt-4o":4,"gpt-4o-mini":4}`,
	GroupRatios:               `{}`,
	DefaultModelRatio:         1,
	DefaultCompletionRatio:    4,
	InputPreConsumeMultiplier: 1.2,
}

// Snapshot 是解析后的只读倍率表。
//
// map 字段不导出：调用方拿不到可写的 map，就无法就地修改已发布的快照。
// 快照一旦 Store 就必须永远不变，否则并发读者会读到半更新的数据。
type Snapshot struct {
	modelRatios               map[string]float64
	completionRatios          map[string]float64
	groupRatios               map[string]float64
	defaultModelRatio         float64
	defaultCompletionRatio    float64
	inputPreConsumeMultiplier float64
}

// ModelRatio 返回模型的输入倍率（美元 / 100 万输入 token）。
// 未配置的模型走兜底值；显式配置为 0 表示免费，必须原样返回。
func (s *Snapshot) ModelRatio(model string) float64 {
	if v, ok := s.modelRatios[model]; ok {
		return v
	}
	return s.defaultModelRatio
}

// CompletionRatio 返回输出相对输入的倍数。未配置的模型走兜底值。
func (s *Snapshot) CompletionRatio(model string) float64 {
	if v, ok := s.completionRatios[model]; ok {
		return v
	}
	return s.defaultCompletionRatio
}

// GroupRatio 返回分组的最终金额倍数。未配置的分组按 1 处理，
// 即不改变金额——分组是可选的分层定价，缺省不应悄悄加价或打折。
func (s *Snapshot) GroupRatio(group string) float64 {
	if v, ok := s.groupRatios[group]; ok {
		return v
	}
	return 1
}

// PreConsumeMultiplier 返回预扣输入费的系数。
// 0 或负数会让预扣失去意义（免费或倒扣），一律回退到 1。
func (s *Snapshot) PreConsumeMultiplier() float64 {
	if s.inputPreConsumeMultiplier <= 0 {
		return 1
	}
	return s.inputPreConsumeMultiplier
}

// registered 是反射写入的目标，只允许配置系统访问。
// 业务代码一律用 Get()——直接读它会读到正在被写入的中间态。
var registered = Default

// current 是热路径读的不可变快照。
var current atomic.Pointer[Snapshot]

func init() {
	// 默认值写错属于开发期错误：启动时直接炸，比带着错误定价上线好。
	snap, err := buildSnapshot(Default)
	if err != nil {
		panic(fmt.Sprintf("ratio_setting: 默认配置无法解析: %v", err))
	}
	current.Store(snap)

	config.GlobalConfig.Register("ratio_setting", &registered)
	config.GlobalConfig.OnRebuild("ratio_setting", func() {
		// 本回调由 config.Manager 在写锁内调用，所以这里读 registered 没有并发写。
		//
		// 先完整解析成一份新快照再原子替换：任何一张表非法都保留旧快照。
		// 若一边解析一边应用，就会出现三张表分属两个版本的状态——按这种状态
		// 计费得到的金额，事后无法用任何一版配置复盘。
		snap, err := buildSnapshot(registered)
		if err != nil {
			logger.Error("ratio_setting 配置非法，保留上一份快照", "error", err)
			return
		}
		current.Store(snap)
	})
}

// Get 返回当前快照。返回值是共享的只读对象，不要试图修改它。
func Get() *Snapshot { return current.Load() }

// ModelRatio 返回当前生效的模型输入倍率（美元 / 100 万输入 token）。
func ModelRatio(model string) float64 { return Get().ModelRatio(model) }

// CompletionRatio 返回当前生效的输出倍数。
func CompletionRatio(model string) float64 { return Get().CompletionRatio(model) }

// GroupRatio 返回当前生效的分组倍数；未配置的分组为 1。
func GroupRatio(group string) float64 { return Get().GroupRatio(group) }

// PreConsumeMultiplier 返回当前生效的预扣系数；配置值 <= 0 时为 1。
func PreConsumeMultiplier() float64 { return Get().PreConsumeMultiplier() }

// buildSnapshot 把 JSON 表解析成一份全新快照。
// 三张表全部解析成功才返回；任一失败返回错误，调用方据此保留旧快照。
func buildSnapshot(s RatioSetting) (*Snapshot, error) {
	var modelRatios map[string]float64
	if err := json.Unmarshal([]byte(s.ModelRatios), &modelRatios); err != nil {
		return nil, fmt.Errorf("解析 model_ratios 失败: %w", err)
	}

	var completionRatios map[string]float64
	if err := json.Unmarshal([]byte(s.CompletionRatios), &completionRatios); err != nil {
		return nil, fmt.Errorf("解析 completion_ratios 失败: %w", err)
	}

	var groupRatios map[string]float64
	if err := json.Unmarshal([]byte(s.GroupRatios), &groupRatios); err != nil {
		return nil, fmt.Errorf("解析 group_ratios 失败: %w", err)
	}

	return &Snapshot{
		modelRatios:               modelRatios,
		completionRatios:          completionRatios,
		groupRatios:               groupRatios,
		defaultModelRatio:         s.DefaultModelRatio,
		defaultCompletionRatio:    s.DefaultCompletionRatio,
		inputPreConsumeMultiplier: s.InputPreConsumeMultiplier,
	}, nil
}
