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
// 负号要在取整**之前**判断：math.Round(-0.4) 是 -0，而 Go 里 -0 < 0 为假，
// 先取整会把"负数金额"当成正常值。三档（Float/Round/Decimal）对同一输入
// 的判定必须一致，否则调用方换个入口就得到相反的结论。
//
// 另注：指数超出 decimal 解码上限的字符串（如 "1e1000000000"）在 decimal 档
// 报 ClampNaN 而不是 ClampOverflow——值从未被成功解析出来，无从与上限比较。
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
	v, _ := QuotaRoundChecked(f)
	return v
}

// QuotaRoundChecked 与 QuotaFromFloatChecked 对同一输入的判定必须一致，
// 所以先判符号再取整（-0.4 取整成 -0 会丢掉符号，详见 clampQuota 的说明）。
func QuotaRoundChecked(f float64) (int, QuotaClamp) {
	if math.IsNaN(f) {
		return 0, ClampNaN
	}
	if f < 0 {
		return 0, ClampUnderflow
	}
	return clampQuota(math.Round(f))
}

func QuotaRoundStrict(f float64) (int, error) {
	v, clamp := QuotaRoundChecked(f)
	if clamp.Saturated() {
		return 0, clampError(clamp)
	}
	return v, nil
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
		// 包上 ErrQuotaNaN：Checked 档对同一输入给 ClampNaN、浮点 Strict 档给
		// ErrQuotaNaN，唯独 strict 这一档拿不到统一的哨兵，调用方就没法用
		// errors.Is 识别"这不是一个有效金额"。
		// shopspring 没有导出解析错误变量，只能把原始错误以文本形式附上。
		return 0, fmt.Errorf("%w：无法解析金额 %q（%v）", ErrQuotaNaN, s, err)
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
	// 负号在取整前判：d.Round(0) 会把 -0.4 变成 0，之后 IsNegative() 就为假了，
	// 于是同一个负数在 decimal 档变成 ClampNone、在浮点档却是 ClampUnderflow。
	if d.IsNegative() {
		return 0, ClampUnderflow
	}
	rounded := d.Round(0) // shopspring 的 Round 是 half away from zero
	if rounded.GreaterThan(decimal.NewFromInt(MaxQuota)) {
		return MaxQuota, ClampOverflow
	}
	return int(rounded.IntPart()), ClampNone
}
