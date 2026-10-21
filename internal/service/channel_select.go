// Package service 是业务逻辑层：选渠道、自动禁用等。
//
// 这一层不碰 HTTP——入参是普通值，出参是 model 与 types，
// 于是它可以在没有 gin、没有数据库连接的测试里被直接验证。
package service

import (
	"math/rand"
	"strings"

	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/model"
	"github.com/yyyCode/whats-api/internal/types"
)

// DefaultGroup 是分组为空时的归一化值。
// 令牌/用户/渠道三处都按它归一化，避免"空串 vs default"这种配置陷阱。
const DefaultGroup = "default"

func NormalizeGroup(g string) string {
	if g = strings.TrimSpace(g); g == "" {
		return DefaultGroup
	}
	return g
}

// SelectChannel 选一个渠道；exclude 是本次请求已经试过（含刚失败）的渠道 ID。
//
// 直查数据库而不是走缓存：M1 的数据量与并发都不值得引入缓存失效问题；
// 缓存 + 反范式索引是 M4 的主题。
func SelectChannel(group, modelName string, exclude map[int]bool) (*model.Channel, *types.APIError) {
	channels, err := model.GetEnabledChannels()
	if err != nil {
		return nil, types.NewInternalError("查询渠道失败: " + err.Error())
	}

	candidates := filterChannels(channels, group, modelName, exclude)
	if len(candidates) == 0 {
		return nil, types.NewNoChannelError(modelName)
	}
	return pickChannel(candidates, rand.Float64()), nil
}

// filterChannels 是纯函数：分组（空=default）、模型清单、排除集、启用态四重过滤。
// 启用态在 SQL 里已经过滤过，这里再判一次是防御——调用方可能传入全表。
func filterChannels(channels []model.Channel, group, modelName string, exclude map[int]bool) []model.Channel {
	g := NormalizeGroup(group)
	out := make([]model.Channel, 0, len(channels))
	for _, ch := range channels {
		if exclude[ch.ID] {
			continue
		}
		if ch.Status != constant.ChannelStatusEnabled {
			continue
		}
		if NormalizeGroup(ch.GroupName) != g {
			continue
		}
		if !modelListContains(ch.Models, modelName) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// modelListContains 按逗号切分并去空白，精确匹配（不做前缀匹配）。
func modelListContains(models, modelName string) bool {
	for _, m := range strings.Split(models, ",") {
		if strings.TrimSpace(m) == modelName {
			return true
		}
	}
	return false
}

// pickChannel：先取最高优先级那一档，再在档内按权重随机；权重全为 0 时均匀随机。
//
// r 是 [0,1) 的随机数，做成参数是为了让分布能被确定性测试——
// 直接调 rand 会让"选中了哪个"只能靠统计，测试既不稳也不快。
func pickChannel(channels []model.Channel, r float64) *model.Channel {
	top := channels[0].Priority
	for _, ch := range channels[1:] {
		if ch.Priority > top {
			top = ch.Priority
		}
	}

	tier := make([]model.Channel, 0, len(channels))
	for _, ch := range channels {
		if ch.Priority == top {
			tier = append(tier, ch)
		}
	}
	if len(tier) == 1 {
		ch := tier[0]
		return &ch
	}

	var total uint64
	for _, ch := range tier {
		total += uint64(ch.Weight)
	}

	// 全 0 权重是合法配置（例如刚建好的渠道）：均匀分布，
	// 既不能 panic，也不能一个都选不出来。
	if total == 0 {
		i := int(r * float64(len(tier)))
		if i >= len(tier) {
			i = len(tier) - 1
		}
		ch := tier[i]
		return &ch
	}

	target := r * float64(total)
	var acc float64
	for i := range tier {
		acc += float64(tier[i].Weight)
		if target < acc {
			ch := tier[i]
			return &ch
		}
	}
	ch := tier[len(tier)-1]
	return &ch
}
