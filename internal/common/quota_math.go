package common

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/shopspring/decimal"
)

// 记账单位：内部货币是整数 quota，永远不用 float 记账（避免累加误差）。
const (
	// QuotaPerUnit 是 1 美元对应的 quota：50 万 quota = 1 美元。
	QuotaPerUnit = 500 * 1000
	// MaxQuota 是单次请求的金额上限，用于防溢出。
	MaxQuota = math.MaxInt32
	// MaxWalletQuota 是钱包/余额上限，取 JS 安全整数上限，保证前端不丢精度。
	MaxWalletQuota = 1<<53 - 1
	// USD2RMB 仅用于展示换算，不参与记账。
	USD2RMB = 7.3
)

// QuotaClamp 记录一次浮点→整数转换中发生的饱和。
//
// 数值异常必须留痕：调用方拿到非 ClampNone 的返回值时必须写进日志
// （AdminMap 提供了现成字段），不允许静默丢弃。
type QuotaClamp uint8

const (
	ClampNone QuotaClamp = iota
	ClampOverflow
	ClampUnderflow
	ClampNaN
)

func (c QuotaClamp) Saturated() bool { return c != ClampNone }

// String 的字面量是日志/审计字段的一部分，改动等于改日志格式。
func (c QuotaClamp) String() string {
	switch c {
	case ClampOverflow:
		return "overflow"
	case ClampUnderflow:
		return "underflow"
	case ClampNaN:
		return "nan"
	default:
		return "none"
	}
}

// AdminMap 供写入日志的 admin_info.quota_saturation 字段，便于管理员事后排查。
func (c QuotaClamp) AdminMap() map[string]any {
	return map[string]any{
		"quota_saturation": c.String(),
		"saturated":        c.Saturated(),
	}
}

// Strict 档的错误。用哨兵错误而不是字符串，因为重试/退款决策要靠 errors.Is。
var (
	ErrQuotaOverflow = errors.New("quota 超过上限")
	ErrQuotaNaN      = errors.New("quota 不是有效数值")
	ErrQuotaNegative = errors.New("quota 为负数")
)

// clampQuota 把已经取整的浮点落到 [0, MaxQuota]，并报告饱和类型。
//
// 语义（本项目的决定，写进账本前请复核）：
//
//	NaN              → 0        + ClampNaN
//	+Inf / > MaxQuota → MaxQuota + ClampOverflow
//	-Inf / < 0        → 0        + ClampUnderflow
//
// 为什么 NaN 不饱和到 MaxQuota：结算路径上那会给出荒谬的巨额扣费。
// 饱和到 0 是"宁可少扣"，但正因为危险，所以必须返回标记让调用方显式处理
// ——零 usage 绝不能当成免费。
func clampQuota(f float64) (int, QuotaClamp) {
	switch {
	case math.IsNaN(f):
		return 0, ClampNaN
	case math.IsInf(f, 1) || f > MaxQuota:
		return MaxQuota, ClampOverflow
	case math.IsInf(f, -1) || f < 0:
		return 0, ClampUnderflow
	}
	return int(f), ClampNone
}

func clampError(c QuotaClamp) error {
	switch c {
	case ClampOverflow:
		return ErrQuotaOverflow
	case ClampUnderflow:
		return ErrQuotaNegative
	case ClampNaN:
		return ErrQuotaNaN
	default:
		return nil
	}
}

// QuotaFromFloat 静默饱和：只用于展示类计算，**不要用于记账**。
// 记账请用 Checked（结算/日志）或 Strict（预扣）。
func QuotaFromFloat(f float64) int {
	v, _ := clampQuota(f)
	return v
}

// QuotaFromFloatChecked 饱和并留痕：用于结算与日志。
func QuotaFromFloatChecked(f float64) (int, QuotaClamp) { return clampQuota(f) }

// QuotaFromFloatStrict 溢出即错：用于预扣——宁可拒绝请求，也不能按错误金额冻结。
func QuotaFromFloatStrict(f float64) (int, error) {
	v, clamp := clampQuota(f)
	if clamp.Saturated() {
		return 0, clampError(clamp)
	}
	return v, nil
}

// QuotaRound 先取整（四舍五入、远离零）再饱和。
//
// 分层计费必须用它：如果先算小数再截断，预扣与结算会差 ±1 quota，
// 日志里天天出现"金额对不上"的噪声。
func QuotaRound(f float64) int {
	v, _ := clampQuota(math.Round(f))
	return v
}

func QuotaRoundChecked(f float64) (int, QuotaClamp) {
	return clampQuota(math.Round(f))
}

func QuotaRoundStrict(f float64) (int, error) {
	return QuotaFromFloatStrict(math.Round(f))
}

// QuotaFromDecimal 用 decimal 处理字符串价格，避免 "0.1" 这类值走 float 丢精度。
// 只用于展示或已确认输入合法的场景；记账请用 Checked / Strict。
func QuotaFromDecimal(s string) int {
	v, _ := QuotaFromDecimalChecked(s)
	return v
}

// QuotaFromDecimalChecked 解析失败或越界时饱和并留痕。
func QuotaFromDecimalChecked(s string) (int, QuotaClamp) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return 0, ClampNaN
	}
	return decimalToQuota(d)
}

// QuotaFromDecimalStrict 解析失败或越界即错（预扣路径）。
func QuotaFromDecimalStrict(s string) (int, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("解析金额 %q 失败: %w", s, err)
	}
	v, clamp := decimalToQuota(d)
	if clamp.Saturated() {
		return 0, clampError(clamp)
	}
	return v, nil
}

// decimalToQuota 把 decimal 四舍五入到整数（远离零）后落到 [0, MaxQuota]。
//
// 注意这里全程不经过 float64：几十位的价格字符串转 float 就丢精度了，
// 越界判断会变得不可靠。
func decimalToQuota(d decimal.Decimal) (int, QuotaClamp) {
	if d.IsZero() {
		return 0, ClampNone
	}
	rounded := d.Round(0) // shopspring 的 Round 是 half away from zero
	if rounded.IsNegative() {
		return 0, ClampUnderflow
	}
	if rounded.GreaterThan(decimal.NewFromInt(MaxQuota)) {
		return MaxQuota, ClampOverflow
	}
	return int(rounded.IntPart()), ClampNone
}
