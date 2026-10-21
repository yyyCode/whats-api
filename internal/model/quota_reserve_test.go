package model

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yyyCode/whats-api/internal/common"
)

// 本文件的用例都只调用一次 withTestDB(t)：它写的是包级单例句柄，
// 每个测试独立建库，测试之间不共享状态。

func createTestUser(t *testing.T, username string, quota, usedQuota int64) *User {
	t.Helper()
	u := &User{Username: username, Quota: quota, UsedQuota: usedQuota}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return u
}

func createTestToken(t *testing.T, key string, userID int, remainQuota, usedQuota int64, unlimited bool) *Token {
	t.Helper()
	tok := &Token{
		Key:            key,
		UserID:         userID,
		RemainQuota:    remainQuota,
		UsedQuota:      usedQuota,
		UnlimitedQuota: unlimited,
	}
	if err := DB.Create(tok).Error; err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	return tok
}

func mustGetUser(t *testing.T, id int) User {
	t.Helper()
	var u User
	if err := DB.First(&u, id).Error; err != nil {
		t.Fatalf("读取用户 %d 失败: %v", id, err)
	}
	return u
}

func mustGetToken(t *testing.T, id int) Token {
	t.Helper()
	var tok Token
	if err := DB.First(&tok, id).Error; err != nil {
		t.Fatalf("读取令牌 %d 失败: %v", id, err)
	}
	return tok
}

// 未初始化主库时必须返回错误而不是空指针崩溃。
func TestQuotaOpsRejectUninitializedDB(t *testing.T) {
	DB, LOG_DB = nil, nil

	if _, err := TryReserveUserQuota(1, 1); err == nil {
		t.Error("DB 为 nil 时 TryReserveUserQuota 应返回错误")
	}
	if _, err := TryReserveTokenQuota(1, 1, false); err == nil {
		t.Error("DB 为 nil 时 TryReserveTokenQuota 应返回错误")
	}
	if _, err := ReserveQuota(1, 1, false, 1); err == nil {
		t.Error("DB 为 nil 时 ReserveQuota 应返回错误")
	}
	if err := SettleQuota(1, 1, false, 1, 1); err == nil {
		t.Error("DB 为 nil 时 SettleQuota 应返回错误")
	}
	if err := RefundQuota(1, 1, false, 1); err == nil {
		t.Error("DB 为 nil 时 RefundQuota 应返回错误")
	}
}

// 单条条件 UPDATE：成功时精确扣减，余额不足时余额不变且返回 false。
func TestTryReserveQuota(t *testing.T) {
	withTestDB(t)

	u := createTestUser(t, "reserve-user", 100, 0)
	tok := createTestToken(t, "reserve-token", u.ID, 100, 0, false)

	ok, err := TryReserveUserQuota(u.ID, 30)
	if err != nil || !ok {
		t.Fatalf("余额充足时预扣失败: ok=%v err=%v", ok, err)
	}
	if got := mustGetUser(t, u.ID).Quota; got != 70 {
		t.Errorf("用户余额 = %d，期望 70", got)
	}

	// 余额只剩 70，扣 71 必须失败，且余额保持 70。
	ok, err = TryReserveUserQuota(u.ID, 71)
	if err != nil {
		t.Fatalf("余额不足不应产生数据库错误: %v", err)
	}
	if ok {
		t.Error("余额不足时预扣结果应为 false")
	}
	if got := mustGetUser(t, u.ID).Quota; got != 70 {
		t.Errorf("失败预扣后余额 = %d，期望保持不变（70）", got)
	}

	ok, err = TryReserveTokenQuota(tok.ID, 30, false)
	if err != nil || !ok {
		t.Fatalf("令牌额度充足时预扣失败: ok=%v err=%v", ok, err)
	}
	if got := mustGetToken(t, tok.ID).RemainQuota; got != 70 {
		t.Errorf("令牌余额 = %d，期望 70", got)
	}

	ok, err = TryReserveTokenQuota(tok.ID, 71, false)
	if err != nil {
		t.Fatalf("令牌余额不足不应产生数据库错误: %v", err)
	}
	if ok {
		t.Error("令牌余额不足时预扣结果应为 false")
	}
	if got := mustGetToken(t, tok.ID).RemainQuota; got != 70 {
		t.Errorf("失败预扣后令牌余额 = %d，期望保持不变（70）", got)
	}

	// 无限令牌与非正金额都是无操作。
	ok, err = TryReserveTokenQuota(tok.ID, 1_000_000, true)
	if err != nil || !ok {
		t.Fatalf("无限令牌预扣应成功: ok=%v err=%v", ok, err)
	}
	if got := mustGetToken(t, tok.ID).RemainQuota; got != 70 {
		t.Errorf("无限令牌余额不该变化，得到 %d", got)
	}
	ok, err = TryReserveUserQuota(u.ID, 0)
	if err != nil || !ok {
		t.Fatalf("quota<=0 应为无操作成功: ok=%v err=%v", ok, err)
	}
	if got := mustGetUser(t, u.ID).Quota; got != 70 {
		t.Errorf("quota<=0 时余额不该变化，得到 %d", got)
	}
}

// 用户余额不足时，先执行的令牌预扣必须随事务回滚。
func TestReserveQuotaRollsBackToken(t *testing.T) {
	withTestDB(t)

	u := createTestUser(t, "rollback-user", 10, 0)
	tok := createTestToken(t, "rollback-token", u.ID, 100, 0, false)

	ok, err := ReserveQuota(u.ID, tok.ID, false, 50)
	if err != nil {
		t.Fatalf("额度不足不应产生数据库错误: %v", err)
	}
	if ok {
		t.Fatal("用户余额不足时 ReserveQuota 应返回 false")
	}
	if got := mustGetUser(t, u.ID).Quota; got != 10 {
		t.Errorf("回滚后用户余额 = %d，期望 10", got)
	}
	if got := mustGetToken(t, tok.ID).RemainQuota; got != 100 {
		t.Errorf("回滚后令牌余额 = %d，期望 100（令牌预扣必须被回滚）", got)
	}

	// 两边都够时，同一个事务里同时扣减。
	u2 := createTestUser(t, "rollback-user-2", 100, 0)
	tok2 := createTestToken(t, "rollback-token-2", u2.ID, 100, 0, false)
	ok, err = ReserveQuota(u2.ID, tok2.ID, false, 30)
	if err != nil || !ok {
		t.Fatalf("额度充足时 ReserveQuota 失败: ok=%v err=%v", ok, err)
	}
	if got := mustGetUser(t, u2.ID).Quota; got != 70 {
		t.Errorf("用户余额 = %d，期望 70", got)
	}
	if got := mustGetToken(t, tok2.ID).RemainQuota; got != 70 {
		t.Errorf("令牌余额 = %d，期望 70", got)
	}
}

// SettleQuota 的三种 delta 符号：quota 与 used_quota 都要精确。
func TestSettleQuota(t *testing.T) {
	withTestDB(t)

	cases := []struct {
		name      string
		delta     int
		actual    int
		wantQuota int64
		wantUsed  int64
	}{
		{name: "delta>0 补扣", delta: 200, actual: 120, wantQuota: 800, wantUsed: 120},
		{name: "delta<0 退回差额", delta: -50, actual: 30, wantQuota: 1050, wantUsed: 30},
		{name: "delta==0 只记用量", delta: 0, actual: 25, wantQuota: 1000, wantUsed: 25},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := createTestUser(t, fmt.Sprintf("settle-user-%d", i), 1000, 0)
			tok := createTestToken(t, fmt.Sprintf("settle-token-%d", i), u.ID, 1000, 0, false)

			if err := SettleQuota(u.ID, tok.ID, false, c.delta, c.actual); err != nil {
				t.Fatalf("SettleQuota 失败: %v", err)
			}

			gotUser := mustGetUser(t, u.ID)
			if gotUser.Quota != c.wantQuota {
				t.Errorf("用户余额 = %d，期望 %d", gotUser.Quota, c.wantQuota)
			}
			if gotUser.UsedQuota != c.wantUsed {
				t.Errorf("用户 used_quota = %d，期望 %d", gotUser.UsedQuota, c.wantUsed)
			}

			gotToken := mustGetToken(t, tok.ID)
			if gotToken.RemainQuota != c.wantQuota {
				t.Errorf("令牌余额 = %d，期望 %d", gotToken.RemainQuota, c.wantQuota)
			}
			if gotToken.UsedQuota != c.wantUsed {
				t.Errorf("令牌 used_quota = %d，期望 %d", gotToken.UsedQuota, c.wantUsed)
			}
		})
	}

	// 退款差额会把用户余额推过钱包上限时，必须报错并回滚整个事务。
	t.Run("delta<0 超过钱包上限报错并回滚", func(t *testing.T) {
		u := createTestUser(t, "settle-overflow-user", common.MaxWalletQuota, 0)
		tok := createTestToken(t, "settle-overflow-token", u.ID, 1000, 0, false)

		if err := SettleQuota(u.ID, tok.ID, false, -1, 5); err == nil {
			t.Fatal("余额加回后将超过上限时 SettleQuota 应返回错误")
		}

		gotUser := mustGetUser(t, u.ID)
		if gotUser.Quota != common.MaxWalletQuota || gotUser.UsedQuota != 0 {
			t.Errorf("事务应回滚：用户 quota=%d used=%d", gotUser.Quota, gotUser.UsedQuota)
		}
		gotToken := mustGetToken(t, tok.ID)
		if gotToken.RemainQuota != 1000 || gotToken.UsedQuota != 0 {
			t.Errorf("事务应回滚：令牌 remain=%d used=%d", gotToken.RemainQuota, gotToken.UsedQuota)
		}
	})
}

// 退款只恢复余额，不改变 used_quota。
func TestRefundQuota(t *testing.T) {
	withTestDB(t)

	u := createTestUser(t, "refund-user", 100, 40)
	tok := createTestToken(t, "refund-token", u.ID, 100, 40, false)

	if err := RefundQuota(u.ID, tok.ID, false, 30); err != nil {
		t.Fatalf("RefundQuota 失败: %v", err)
	}

	gotUser := mustGetUser(t, u.ID)
	if gotUser.Quota != 130 {
		t.Errorf("用户余额 = %d，期望 130", gotUser.Quota)
	}
	if gotUser.UsedQuota != 40 {
		t.Errorf("退款不该改 used_quota，用户 used_quota = %d，期望 40", gotUser.UsedQuota)
	}

	gotToken := mustGetToken(t, tok.ID)
	if gotToken.RemainQuota != 130 {
		t.Errorf("令牌余额 = %d，期望 130", gotToken.RemainQuota)
	}
	if gotToken.UsedQuota != 40 {
		t.Errorf("退款不该改 used_quota，令牌 used_quota = %d，期望 40", gotToken.UsedQuota)
	}

	// amount<=0 是无操作。
	if err := RefundQuota(u.ID, tok.ID, false, 0); err != nil {
		t.Fatalf("amount=0 不应报错: %v", err)
	}
	if got := mustGetUser(t, u.ID).Quota; got != 130 {
		t.Errorf("amount=0 时余额不该变化，得到 %d", got)
	}

	// 无限令牌只退用户钱包。
	u2 := createTestUser(t, "refund-user-unlimited", 100, 0)
	tok2 := createTestToken(t, "refund-token-unlimited", u2.ID, 100, 0, true)
	if err := RefundQuota(u2.ID, tok2.ID, true, 20); err != nil {
		t.Fatalf("无限令牌 RefundQuota 失败: %v", err)
	}
	if got := mustGetUser(t, u2.ID).Quota; got != 120 {
		t.Errorf("用户余额 = %d，期望 120", got)
	}
	if got := mustGetToken(t, tok2.ID).RemainQuota; got != 100 {
		t.Errorf("无限令牌余额不该变化，得到 %d", got)
	}
}

// runConcurrentReserve 并发执行 attempts 次 reserve，返回成功次数。
func runConcurrentReserve(t *testing.T, attempts int, reserve func() (bool, error)) int64 {
	t.Helper()

	var success atomic.Int64
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			ok, err := reserve()
			if err != nil {
				t.Errorf("并发预扣返回错误: %v", err)
				return
			}
			if ok {
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	return success.Load()
}

// 200 个并发请求抢 100 quota：条件 UPDATE 必须让恰好 100 个成功，余额归零且不为负。
// 这里不能用 SELECT-then-UPDATE，否则会成功超过 100 次或余额变负。
func TestConcurrentReserveUserQuotaNeverOverdraws(t *testing.T) {
	withTestDB(t)

	u := createTestUser(t, "concurrent-user", 100, 0)

	const attempts = 200
	success := runConcurrentReserve(t, attempts, func() (bool, error) {
		return TryReserveUserQuota(u.ID, 1)
	})

	if success != 100 {
		t.Errorf("成功预扣次数 = %d，期望恰好 100", success)
	}
	got := mustGetUser(t, u.ID)
	if got.Quota != 0 {
		t.Errorf("最终用户余额 = %d，期望 0", got.Quota)
	}
	if got.Quota < 0 {
		t.Errorf("用户余额被透支: %d", got.Quota)
	}
}

// 令牌版本同样断言：200 并发抢 remain_quota=100，恰好 100 次成功。
func TestConcurrentReserveTokenQuotaNeverOverdraws(t *testing.T) {
	withTestDB(t)

	u := createTestUser(t, "concurrent-token-user", 0, 0)
	tok := createTestToken(t, "concurrent-token", u.ID, 100, 0, false)

	const attempts = 200
	success := runConcurrentReserve(t, attempts, func() (bool, error) {
		return TryReserveTokenQuota(tok.ID, 1, false)
	})

	if success != 100 {
		t.Errorf("成功预扣次数 = %d，期望恰好 100", success)
	}
	got := mustGetToken(t, tok.ID)
	if got.RemainQuota != 0 {
		t.Errorf("最终令牌余额 = %d，期望 0", got.RemainQuota)
	}
	if got.RemainQuota < 0 {
		t.Errorf("令牌余额被透支: %d", got.RemainQuota)
	}
}
