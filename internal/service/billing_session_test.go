package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/model"
)

// withBillingDB 为单个测试建立真实临时 SQLite 主库。
//
// ★ t.Cleanup(common.InitEnv) 必须最先注册：t.Cleanup 是 LIFO，
// 它因此最后执行，此时 t.Setenv 已经还原了环境变量，再跑一次 InitEnv
// 才能把 common 的包级全局（SQLDSN / NodeType / IsMasterNode）恢复成
// 进程初始值。否则本测试设的 master 节点配置会泄漏给同包后续用例。
//
// model.withTestDB 是 model 包私有 helper，service 包无法直接复用，
// 所以这里保留同样的关键步骤：临时文件、master 节点（否则迁移静默 no-op）、
// InitDB 后注册 CloseDB 收尾。本包用例不并行执行。
func withBillingDB(t *testing.T) {
	t.Helper()

	t.Cleanup(common.InitEnv)

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "billing.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	common.InitEnv()

	if err := model.InitDB(); err != nil {
		t.Fatalf("InitDB 失败: %v", err)
	}
	t.Cleanup(func() { _ = model.CloseDB() })
}

// billingSeq 保证同一进程内（即使将来出现子测试共享数据库）用户名与 token key 唯一。
var billingSeq atomic.Int64

// seedBillingFixture 创建一个启用的用户与一个启用的令牌，返回它们的 ID。
func seedBillingFixture(t *testing.T, userQuota, tokenRemain int64, unlimited bool) (userID, tokenID int) {
	t.Helper()

	n := billingSeq.Add(1)
	u := &model.User{
		Username: fmt.Sprintf("billing-user-%d", n),
		Status:   constant.UserStatusEnabled,
		Quota:    userQuota,
	}
	if err := model.DB.Create(u).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	tok := &model.Token{
		Key:            fmt.Sprintf("billing-token-%d", n),
		UserID:         u.ID,
		Status:         constant.TokenStatusEnabled,
		RemainQuota:    tokenRemain,
		UnlimitedQuota: unlimited,
	}
	if err := model.DB.Create(tok).Error; err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}

	return u.ID, tok.ID
}

// readBillingUser 每次都读入新的结构体。
//
// ★ 不能复用同一个变量：GORM 的 First 会把主键合并进目标结构体，
// 复用时查询条件会被上一次的主键污染，读到错行或直接读不到。
func readBillingUser(t *testing.T, id int) model.User {
	t.Helper()
	var u model.User
	if err := model.DB.First(&u, id).Error; err != nil {
		t.Fatalf("读取用户 %d 失败: %v", id, err)
	}
	return u
}

// readBillingToken 同 readBillingUser，每次使用独立结构体。
func readBillingToken(t *testing.T, id int) model.Token {
	t.Helper()
	var tok model.Token
	if err := model.DB.First(&tok, id).Error; err != nil {
		t.Fatalf("读取令牌 %d 失败: %v", id, err)
	}
	return tok
}

// 正常路径：预扣 → 补扣差额结算 → 重复结算与结算后退款都是无害 no-op。
func TestBillingPreConsumeAndSettle(t *testing.T) {
	withBillingDB(t)

	userID, tokenID := seedBillingFixture(t, 1000, 500, false)
	s := NewBillingSession(userID, tokenID, false)

	got, err := s.PreConsume(300)
	if err != nil {
		t.Fatalf("PreConsume 失败: %v", err)
	}
	if got != 300 {
		t.Errorf("PreConsume 返回 %d，期望 300", got)
	}
	if s.State() != BillingPreConsumed {
		t.Errorf("状态 = %s，期望 preConsumed", s.State())
	}
	if s.PreConsumed() != 300 {
		t.Errorf("PreConsumed() = %d，期望 300", s.PreConsumed())
	}

	u := readBillingUser(t, userID)
	if u.Quota != 700 {
		t.Errorf("预扣后用户余额 = %d，期望 700", u.Quota)
	}
	tok := readBillingToken(t, tokenID)
	if tok.RemainQuota != 200 {
		t.Errorf("预扣后令牌余额 = %d，期望 200", tok.RemainQuota)
	}
	if u.UsedQuota != 0 || tok.UsedQuota != 0 {
		t.Errorf("预扣不该累加 used_quota：user=%d token=%d", u.UsedQuota, tok.UsedQuota)
	}

	// 第二次预扣是非法转移：必须报错，且不能动账本。
	if _, err := s.PreConsume(1); err == nil {
		t.Error("第二次 PreConsume 应返回错误")
	}
	if s.State() != BillingPreConsumed {
		t.Errorf("非法预扣后状态 = %s，期望仍是 preConsumed", s.State())
	}
	if u := readBillingUser(t, userID); u.Quota != 700 {
		t.Errorf("非法预扣后用户余额 = %d，期望仍为 700", u.Quota)
	}

	// preConsumed=300，actual=400：delta=+100，用户 700→600、令牌 200→100。
	ok, err := s.Settle(400)
	if err != nil || !ok {
		t.Fatalf("Settle(400) = (%v, %v)，期望 (true, nil)", ok, err)
	}
	if s.State() != BillingSettled {
		t.Errorf("结算后状态 = %s，期望 settled", s.State())
	}

	u = readBillingUser(t, userID)
	if u.Quota != 600 {
		t.Errorf("结算后用户余额 = %d，期望 600", u.Quota)
	}
	if u.UsedQuota != 400 {
		t.Errorf("结算后用户 used_quota = %d，期望 400", u.UsedQuota)
	}
	tok = readBillingToken(t, tokenID)
	if tok.RemainQuota != 100 {
		t.Errorf("结算后令牌余额 = %d，期望 100", tok.RemainQuota)
	}
	if tok.UsedQuota != 400 {
		t.Errorf("结算后令牌 used_quota = %d，期望 400", tok.UsedQuota)
	}

	// 重复结算：正常幂等调用，返回 (false, nil)，账本不变。
	ok, err = s.Settle(400)
	if err != nil || ok {
		t.Errorf("重复 Settle = (%v, %v)，期望 (false, nil)", ok, err)
	}
	if u := readBillingUser(t, userID); u.Quota != 600 || u.UsedQuota != 400 {
		t.Errorf("重复结算改动了用户账本：quota=%d used=%d", u.Quota, u.UsedQuota)
	}
	if tok := readBillingToken(t, tokenID); tok.RemainQuota != 100 || tok.UsedQuota != 400 {
		t.Errorf("重复结算改动了令牌账本：remain=%d used=%d", tok.RemainQuota, tok.UsedQuota)
	}

	// 结算之后退款必须无效：否则同一笔请求既计费又退钱。
	ok, err = s.Refund()
	if err != nil || ok {
		t.Errorf("结算后 Refund = (%v, %v)，期望 (false, nil)", ok, err)
	}
	if u := readBillingUser(t, userID); u.Quota != 600 {
		t.Errorf("结算后退款改动了用户余额：%d，期望 600", u.Quota)
	}
	if tok := readBillingToken(t, tokenID); tok.RemainQuota != 100 {
		t.Errorf("结算后退款改动了令牌余额：%d，期望 100", tok.RemainQuota)
	}
}

// 免费模型（quota<=0）不碰账本，但仍要走完 init → preConsumed → settled。
func TestBillingPreConsumeFreeQuota(t *testing.T) {
	withBillingDB(t)

	userID, tokenID := seedBillingFixture(t, 1000, 500, false)
	s := NewBillingSession(userID, tokenID, false)

	got, err := s.PreConsume(0)
	if err != nil || got != 0 {
		t.Fatalf("PreConsume(0) = (%d, %v)，期望 (0, nil)", got, err)
	}
	if s.State() != BillingPreConsumed {
		t.Errorf("免费模型状态 = %s，期望 preConsumed（仍须走状态机）", s.State())
	}
	if s.PreConsumed() != 0 {
		t.Errorf("PreConsumed() = %d，期望 0", s.PreConsumed())
	}

	ok, err := s.Settle(0)
	if err != nil || !ok {
		t.Fatalf("免费模型 Settle(0) = (%v, %v)，期望 (true, nil)", ok, err)
	}
	if s.State() != BillingSettled {
		t.Errorf("免费模型结算后状态 = %s，期望 settled", s.State())
	}

	u := readBillingUser(t, userID)
	tok := readBillingToken(t, tokenID)
	if u.Quota != 1000 || tok.RemainQuota != 500 || u.UsedQuota != 0 || tok.UsedQuota != 0 {
		t.Errorf("免费模型账本不该变化：user(quota=%d used=%d) token(remain=%d used=%d)",
			u.Quota, u.UsedQuota, tok.RemainQuota, tok.UsedQuota)
	}
}

// 退款路径：第一次退款成功，第二次是幂等 no-op；余额回到原值，used_quota 不变。
func TestBillingRefundIsIdempotent(t *testing.T) {
	withBillingDB(t)

	userID, tokenID := seedBillingFixture(t, 1000, 500, false)
	s := NewBillingSession(userID, tokenID, false)

	if _, err := s.PreConsume(300); err != nil {
		t.Fatalf("PreConsume 失败: %v", err)
	}
	if u := readBillingUser(t, userID); u.Quota != 700 {
		t.Fatalf("预扣后用户余额 = %d，期望 700", u.Quota)
	}

	ok, err := s.Refund()
	if err != nil || !ok {
		t.Fatalf("第一次 Refund = (%v, %v)，期望 (true, nil)", ok, err)
	}
	if s.State() != BillingRefunded {
		t.Errorf("退款后状态 = %s，期望 refunded", s.State())
	}

	ok, err = s.Refund()
	if err != nil || ok {
		t.Errorf("第二次 Refund = (%v, %v)，期望 (false, nil)", ok, err)
	}

	u := readBillingUser(t, userID)
	tok := readBillingToken(t, tokenID)
	if u.Quota != 1000 || tok.RemainQuota != 500 {
		t.Errorf("退款后余额未复原：user=%d token=%d，期望 1000/500", u.Quota, tok.RemainQuota)
	}
	if u.UsedQuota != 0 || tok.UsedQuota != 0 {
		t.Errorf("退款不该改动 used_quota：user=%d token=%d", u.UsedQuota, tok.UsedQuota)
	}
}

// 预扣失败：errors.Is 能识别 ErrInsufficientQuota，余额不变，状态留在 init，
// 之后的 Refund 是 no-op（调用方的 defer 可以无条件调用它）。
func TestBillingPreConsumeInsufficientQuota(t *testing.T) {
	withBillingDB(t)

	// 用户余额够，令牌额度只有 200：任一资金源不足都必须整体失败。
	userID, tokenID := seedBillingFixture(t, 1000, 200, false)
	s := NewBillingSession(userID, tokenID, false)

	_, err := s.PreConsume(300)
	if err == nil {
		t.Fatal("令牌额度不足时 PreConsume 应返回错误")
	}
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Errorf("错误应可 errors.Is(ErrInsufficientQuota)：%v", err)
	}
	if s.State() != BillingInit {
		t.Errorf("预扣失败后状态 = %s，期望 init", s.State())
	}
	if s.PreConsumed() != 0 {
		t.Errorf("预扣失败后 PreConsumed() = %d，期望 0", s.PreConsumed())
	}

	u := readBillingUser(t, userID)
	tok := readBillingToken(t, tokenID)
	if u.Quota != 1000 || tok.RemainQuota != 200 {
		t.Errorf("预扣失败后余额应不变：user=%d token=%d，期望 1000/200", u.Quota, tok.RemainQuota)
	}

	ok, err := s.Refund()
	if err != nil || ok {
		t.Errorf("init 状态 Refund = (%v, %v)，期望 (false, nil)", ok, err)
	}
}

// 结算差额为负：actual < preConsumed 时退回差额，used_quota 按 actual 记。
func TestBillingSettleNegativeDelta(t *testing.T) {
	withBillingDB(t)

	userID, tokenID := seedBillingFixture(t, 1000, 500, false)
	s := NewBillingSession(userID, tokenID, false)

	if _, err := s.PreConsume(300); err != nil {
		t.Fatalf("PreConsume 失败: %v", err)
	}

	// 预扣后 700/200；actual=100 → delta=-200，退回 200 → 900/400。
	ok, err := s.Settle(100)
	if err != nil || !ok {
		t.Fatalf("Settle(100) = (%v, %v)，期望 (true, nil)", ok, err)
	}

	u := readBillingUser(t, userID)
	if u.Quota != 900 {
		t.Errorf("结算后用户余额 = %d，期望 900", u.Quota)
	}
	if u.UsedQuota != 100 {
		t.Errorf("结算后用户 used_quota = %d，期望 100", u.UsedQuota)
	}
	tok := readBillingToken(t, tokenID)
	if tok.RemainQuota != 400 {
		t.Errorf("结算后令牌余额 = %d，期望 400", tok.RemainQuota)
	}
	if tok.UsedQuota != 100 {
		t.Errorf("结算后令牌 used_quota = %d，期望 100", tok.UsedQuota)
	}
}

// 无限令牌只扣用户钱包：令牌的 remain_quota 与 used_quota 都不参与记账。
func TestBillingUnlimitedToken(t *testing.T) {
	withBillingDB(t)

	userID, tokenID := seedBillingFixture(t, 1000, 500, true)
	s := NewBillingSession(userID, tokenID, true)

	if _, err := s.PreConsume(300); err != nil {
		t.Fatalf("PreConsume 失败: %v", err)
	}
	if u := readBillingUser(t, userID); u.Quota != 700 {
		t.Errorf("预扣后用户余额 = %d，期望 700", u.Quota)
	}
	if tok := readBillingToken(t, tokenID); tok.RemainQuota != 500 {
		t.Errorf("无限令牌余额不该变化：%d，期望 500", tok.RemainQuota)
	}

	// preConsumed=300、actual=200 → 用户再退回 100，净扣 200。
	ok, err := s.Settle(200)
	if err != nil || !ok {
		t.Fatalf("Settle(200) = (%v, %v)，期望 (true, nil)", ok, err)
	}

	u := readBillingUser(t, userID)
	if u.Quota != 800 {
		t.Errorf("结算后用户余额 = %d，期望 800", u.Quota)
	}
	if u.UsedQuota != 200 {
		t.Errorf("结算后用户 used_quota = %d，期望 200", u.UsedQuota)
	}
	// 无限令牌不动 remain_quota，但 used_quota 仍要累加：
	// 否则每把无限 key 的消费报表永远是 0，按 key 观察用量就失效了。
	tok := readBillingToken(t, tokenID)
	if tok.RemainQuota != 500 || tok.UsedQuota != 200 {
		t.Errorf("无限令牌账本 = (remain %d, used %d)，期望 500/200", tok.RemainQuota, tok.UsedQuota)
	}
}

// 同一会话被多个 goroutine 同时结算：互斥锁必须让恰好一次生效，
// 最终账本等价于单次 Settle(400)。
func TestBillingConcurrentSettleOnlyOnce(t *testing.T) {
	withBillingDB(t)

	userID, tokenID := seedBillingFixture(t, 1000, 500, false)
	s := NewBillingSession(userID, tokenID, false)

	if _, err := s.PreConsume(300); err != nil {
		t.Fatalf("PreConsume 失败: %v", err)
	}

	const goroutines = 10
	var success atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, err := s.Settle(400)
			if err != nil {
				t.Errorf("并发 Settle 返回错误: %v", err)
				return
			}
			if ok {
				success.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Errorf("并发 Settle 成功次数 = %d，期望恰好 1", got)
	}
	if s.State() != BillingSettled {
		t.Errorf("并发结算后状态 = %s，期望 settled", s.State())
	}

	// 与 TestBillingPreConsumeAndSettle 相同的最终账本：只补扣一次 100。
	u := readBillingUser(t, userID)
	if u.Quota != 600 || u.UsedQuota != 400 {
		t.Errorf("并发结算后用户账本 quota=%d used=%d，期望 600/400", u.Quota, u.UsedQuota)
	}
	tok := readBillingToken(t, tokenID)
	if tok.RemainQuota != 100 || tok.UsedQuota != 400 {
		t.Errorf("并发结算后令牌账本 remain=%d used=%d，期望 100/400", tok.RemainQuota, tok.UsedQuota)
	}
}
