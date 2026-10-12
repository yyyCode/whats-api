package common

import (
	"errors"
	"math"
	"testing"
)

// Checked 档：饱和并留痕（结算、日志用）。
func TestQuotaFromFloatCheckedSaturates(t *testing.T) {
	cases := []struct {
		name  string
		in    float64
		want  int
		clamp QuotaClamp
	}{
		{"正常值", 123, 123, ClampNone},
		{"零", 0, 0, ClampNone},
		{"恰好上限", float64(MaxQuota), MaxQuota, ClampNone},
		{"超过上限", float64(MaxQuota) + 1, MaxQuota, ClampOverflow},
		{"正无穷", math.Inf(1), MaxQuota, ClampOverflow},
		{"NaN", math.NaN(), 0, ClampNaN},
		{"负数", -1, 0, ClampUnderflow},
		{"负无穷", math.Inf(-1), 0, ClampUnderflow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, clamp := QuotaFromFloatChecked(c.in)
			if got != c.want || clamp != c.clamp {
				t.Errorf("QuotaFromFloatChecked(%v) = (%d, %v)，期望 (%d, %v)",
					c.in, got, clamp, c.want, c.clamp)
			}
		})
	}
}

// NaN 必须饱和到 0 而不是上限：结算时饱和到上限会给出荒谬的巨额扣费。
// 但 0 是危险的，所以必须带 ClampNaN 让调用方显式处理（退款/告警）。
func TestNaNDoesNotSaturateToMaxQuota(t *testing.T) {
	got, clamp := QuotaFromFloatChecked(math.NaN())
	if got == MaxQuota {
		t.Fatal("NaN 饱和到上限会在结算时造成巨额误扣")
	}
	if !clamp.Saturated() {
		t.Fatal("NaN 必须留下饱和标记，否则调用方会静默按 0 计费")
	}
}

// 基础档静默饱和：调用方无从得知，所以只能用于展示类计算。
func TestQuotaFromFloatIsSilent(t *testing.T) {
	if got := QuotaFromFloat(math.NaN()); got != 0 {
		t.Errorf("QuotaFromFloat(NaN) = %d，期望 0", got)
	}
	if got := QuotaFromFloat(math.Inf(1)); got != MaxQuota {
		t.Errorf("QuotaFromFloat(+Inf) = %d，期望 %d", got, MaxQuota)
	}
	if got := QuotaFromFloat(1.9); got != 1 {
		t.Errorf("QuotaFromFloat 不取整，1.9 应截断为 1，得到 %d", got)
	}
}

// Strict 档：溢出即错（预扣用）。宁可拒绝请求，也不能按错误金额冻结。
func TestQuotaFromFloatStrictRejectsSaturation(t *testing.T) {
	cases := []struct {
		name    string
		in      float64
		wantErr error
	}{
		{"超过上限", math.Inf(1), ErrQuotaOverflow},
		{"NaN", math.NaN(), ErrQuotaNaN},
		{"负数", -0.5, ErrQuotaNegative},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := QuotaFromFloatStrict(c.in)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("期望错误 %v，得到 %v", c.wantErr, err)
			}
			if v != 0 {
				t.Errorf("出错时必须返回 0，得到 %d", v)
			}
		})
	}

	v, err := QuotaFromFloatStrict(42)
	if err != nil || v != 42 {
		t.Fatalf("正常值不该报错：v=%d err=%v", v, err)
	}
}

// 分层计费必须用 QuotaRound：否则预扣与结算会差 ±1 quota，日志里天天有噪声差异。
func TestQuotaRoundIsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{
		{0.4, 0},
		{0.5, 1},
		{1.5, 2},
		{2.5, 3},
		{100.49, 100},
		{100.5, 101},
	}
	for _, c := range cases {
		if got := QuotaRound(c.in); got != c.want {
			t.Errorf("QuotaRound(%v) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 负数取整后落到下溢分支，返回 0 并带标记——而不是得到负数金额。
func TestQuotaRoundCheckedNegativeUnderflows(t *testing.T) {
	got, clamp := QuotaRoundChecked(-0.5)
	if got != 0 || clamp != ClampUnderflow {
		t.Errorf("QuotaRoundChecked(-0.5) = (%d, %v)，期望 (0, %v)", got, clamp, ClampUnderflow)
	}

	if v, err := QuotaRoundStrict(-0.5); !errors.Is(err, ErrQuotaNegative) || v != 0 {
		t.Errorf("QuotaRoundStrict(-0.5) = (%d, %v)，期望 (0, ErrQuotaNegative)", v, err)
	}
}

// -0.4 取整后是 -0，而 -0 < 0 为假：负数必须在取整前认出，
// 否则 Round 档与 Float 档对同一个负数会给出相反结论。
func TestQuotaRoundAgreesWithFloatFamilyOnNegativeFractions(t *testing.T) {
	for _, f := range []float64{-0.4, -0.4999, -0.0001} {
		wantV, wantClamp := QuotaFromFloatChecked(f)
		gotV, gotClamp := QuotaRoundChecked(f)
		if gotV != wantV || gotClamp != wantClamp {
			t.Errorf("QuotaRoundChecked(%v) = (%d, %v)，而 QuotaFromFloatChecked(%v) = (%d, %v)",
				f, gotV, gotClamp, f, wantV, wantClamp)
		}
		if _, err := QuotaRoundStrict(f); !errors.Is(err, ErrQuotaNegative) {
			t.Errorf("QuotaRoundStrict(%v) 应报 ErrQuotaNegative，得到 %v", f, err)
		}
	}

	// 精确的 0 与 -0 都不是负数，不该被当成下溢。
	for _, f := range []float64{0, math.Copysign(0, -1)} {
		if v, clamp := QuotaRoundChecked(f); v != 0 || clamp != ClampNone {
			t.Errorf("QuotaRoundChecked(%v) = (%d, %v)，期望 (0, ClampNone)", f, v, clamp)
		}
	}
}

// decimal 档与浮点档对负数的判定也必须一致。
func TestDecimalAgreesWithFloatFamilyOnNegativeFractions(t *testing.T) {
	for _, s := range []string{"-0.4", "-0.0001"} {
		if got, clamp := QuotaFromDecimalChecked(s); got != 0 || clamp != ClampUnderflow {
			t.Errorf("QuotaFromDecimalChecked(%q) = (%d, %v)，期望 (0, ClampUnderflow)", s, got, clamp)
		}
	}
	if got, clamp := QuotaFromDecimalChecked("-0"); got != 0 || clamp != ClampNone {
		t.Errorf("QuotaFromDecimalChecked(\"-0\") = (%d, %v)，期望 (0, ClampNone)", got, clamp)
	}
}

func TestQuotaFromDecimal(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  int
		clamp QuotaClamp
	}{
		{"整数", "1000", 1000, ClampNone},
		{"四舍五入进位", "10.5", 11, ClampNone},
		{"四舍五入舍去", "10.4", 10, ClampNone},
		{"零", "0", 0, ClampNone},
		{"负数", "-1", 0, ClampUnderflow},
		{"超过上限", "99999999999999999999", MaxQuota, ClampOverflow},
		{"负数（取整后为 0）", "-0.4", 0, ClampUnderflow},
		{"负零", "-0", 0, ClampNone},
		{"非数字", "abc", 0, ClampNaN},
		{"空串", "", 0, ClampNaN},
		{"前后空白", "  12.6  ", 13, ClampNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, clamp := QuotaFromDecimalChecked(c.in)
			if got != c.want || clamp != c.clamp {
				t.Errorf("QuotaFromDecimalChecked(%q) = (%d, %v)，期望 (%d, %v)",
					c.in, got, clamp, c.want, c.clamp)
			}
		})
	}
}

// 超大数字必须走 decimal 判定：转成 float64 会丢精度，越界判断就不可靠了。
func TestQuotaFromDecimalStrictRejectsBadInput(t *testing.T) {
	v, err := QuotaFromDecimalStrict("99999999999999999999")
	if !errors.Is(err, ErrQuotaOverflow) || v != 0 {
		t.Errorf("超上限应报 ErrQuotaOverflow，得到 (%d, %v)", v, err)
	}
	if _, err := QuotaFromDecimalStrict("abc"); err == nil {
		t.Error("非数字应报错")
	}
	if _, err := QuotaFromDecimalStrict("-1"); !errors.Is(err, ErrQuotaNegative) {
		t.Errorf("负数应报 ErrQuotaNegative，得到 %v", err)
	}
	if v, err := QuotaFromDecimalStrict("12.5"); err != nil || v != 13 {
		t.Errorf("QuotaFromDecimalStrict(12.5) = (%d, %v)，期望 (13, nil)", v, err)
	}
}

func TestQuotaClampAdminMap(t *testing.T) {
	m := ClampOverflow.AdminMap()
	if m["quota_saturation"] != "overflow" || m["saturated"] != true {
		t.Errorf("ClampOverflow.AdminMap() = %v", m)
	}
	if ClampNone.AdminMap()["saturated"] != false {
		t.Error("ClampNone 不应标记为饱和")
	}
	if ClampNone.String() != "none" || ClampOverflow.String() != "overflow" ||
		ClampUnderflow.String() != "underflow" || ClampNaN.String() != "nan" {
		t.Error("QuotaClamp.String() 的字面量是日志字段的一部分，不能随意改")
	}
	if ClampNone.Saturated() {
		t.Error("ClampNone.Saturated() 应为 false")
	}
}

// 钱包上限取 JS 安全整数上限，否则前端展示会丢精度。
func TestWalletQuotaFitsJSSafeInteger(t *testing.T) {
	if MaxWalletQuota != 9007199254740991 {
		t.Errorf("MaxWalletQuota = %d，期望 9007199254740991 (1<<53-1)", MaxWalletQuota)
	}
	if QuotaPerUnit != 500000 {
		t.Errorf("QuotaPerUnit = %d，期望 500000（50 万 quota = 1 美元）", QuotaPerUnit)
	}
	if MaxQuota != math.MaxInt32 {
		t.Errorf("MaxQuota = %d，期望 math.MaxInt32", MaxQuota)
	}
}
