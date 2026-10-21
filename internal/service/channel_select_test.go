package service

import (
	"testing"

	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/model"
)

func TestFilterChannels(t *testing.T) {
	channels := []model.Channel{
		{ID: 1, Status: constant.ChannelStatusEnabled, GroupName: "default", Models: "gpt-4o, gpt-4o-mini"},
		{ID: 2, Status: constant.ChannelStatusEnabled, GroupName: "", Models: "gpt-4o"}, // 空分组按 default
		{ID: 3, Status: constant.ChannelStatusEnabled, GroupName: "vip", Models: "gpt-4o"},
		{ID: 4, Status: constant.ChannelStatusManuallyDisabled, GroupName: "default", Models: "gpt-4o"},
		{ID: 5, Status: constant.ChannelStatusEnabled, GroupName: "default", Models: "gpt-4o-mini"},
	}

	got := filterChannels(channels, "default", "gpt-4o", map[int]bool{1: true})
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("候选 = %+v，期望只剩 ID=2", got)
	}

	if !modelListContains("a, b ,c", "b") {
		t.Error("模型列表应按逗号切分并去空白")
	}
	if modelListContains("gpt-4o", "gpt-4") {
		t.Error("模型匹配必须精确，不能是前缀")
	}
}

func TestPickChannelPrefersHighestPriority(t *testing.T) {
	channels := []model.Channel{
		{ID: 1, Priority: 0, Weight: 100},
		{ID: 2, Priority: 10, Weight: 1},
	}
	for _, r := range []float64{0, 0.5, 0.999} {
		if got := pickChannel(channels, r); got.ID != 2 {
			t.Errorf("r=%v 选中 %d，期望最高优先级的 2", r, got.ID)
		}
	}
}

func TestPickChannelWeightedWithinTier(t *testing.T) {
	channels := []model.Channel{
		{ID: 1, Priority: 5, Weight: 1},
		{ID: 2, Priority: 5, Weight: 3},
	}
	cases := []struct {
		r    float64
		want int
	}{
		{0.0, 1},
		{0.24, 1},
		{0.25, 2},
		{0.99, 2},
	}
	for _, c := range cases {
		if got := pickChannel(channels, c.r); got.ID != c.want {
			t.Errorf("r=%v 选中 %d，期望 %d", c.r, got.ID, c.want)
		}
	}
}

func TestPickChannelAllZeroWeightsFallsBackToUniform(t *testing.T) {
	channels := []model.Channel{
		{ID: 1, Priority: 1, Weight: 0},
		{ID: 2, Priority: 1, Weight: 0},
		{ID: 3, Priority: 1, Weight: 0},
	}
	if got := pickChannel(channels, 0.99); got.ID != 3 {
		t.Errorf("全 0 权重应退化为均匀分布，r=0.99 期望 3，实际 %d", got.ID)
	}
}

func TestNormalizeGroup(t *testing.T) {
	if NormalizeGroup("") != "default" || NormalizeGroup("  ") != "default" {
		t.Error("空分组应归一化为 default")
	}
	if NormalizeGroup("vip") != "vip" {
		t.Error("非空分组应原样保留")
	}
}
