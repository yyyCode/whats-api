package service

import (
	"errors"
	"fmt"
	"sync"

	"github.com/yyyCode/whats-api/internal/model"
)

// BillingState 是一次请求计费生命周期的显式状态。
//
// ★ 为什么用状态机而不是四个 bool（preConsumed / settled / refunded / failed）：
// 四个 bool 有 16 种组合，其中只有 4 种是合法的；任何一段代码都可以把它们
// 改成 "settled 和 refunded 同时为真" 或 "既没预扣又已结算" 这种无法解释的
// 状态，而且编译器和测试都很难穷举。一个枚举把非法状态从"运行时的可能性"
// 降成"不存在"：每次转移都在同一把锁内检查前置状态，账本操作与状态更新
// 因此是原子的，后到的 defer 不会重复扣款或重复退款。
type BillingState int

const (
	// BillingInit 是新建会话的初始状态：尚未冻结任何额度。
	BillingInit BillingState = iota
	// BillingPreConsumed 表示预扣已成功，等待结算或退款。
	BillingPreConsumed
	// BillingSettled 是终态：已按真实用量记过账。
	BillingSettled
	// BillingRefunded 是终态：预扣金额已原样退回。
	BillingRefunded
)

// String 只用于错误信息与测试日志，让状态机转移失败时能看懂当前状态。
func (s BillingState) String() string {
	switch s {
	case BillingInit:
		return "init"
	case BillingPreConsumed:
		return "preConsumed"
	case BillingSettled:
		return "settled"
	case BillingRefunded:
		return "refunded"
	default:
		return fmt.Sprintf("BillingState(%d)", int(s))
	}
}

// ErrInsufficientQuota 表示用户钱包或令牌额度的任意一侧不足以完成预扣。
//
// 调用方（HTTP 层）据此映射成 403/quota 错误；用哨兵错误而不是字符串匹配，
// 是为了让映射不受错误文案调整的影响。
var ErrInsufficientQuota = errors.New("额度不足")

// BillingSession 是一次转发请求的计费状态机。
//
// 一个请求从预扣开始，最终只会走向结算或退款之一。所有字段都不导出：
// 状态只能通过方法迁移，外部不可能绕过锁把 state 与数据库账本改得不一致。
// mu 同时保护 state 与 preConsumed——Settle/Refund 需要在"检查前置状态"与
// "写入数据库"之间保持互斥，否则两个 defer 可能同时看到 preConsumed。
type BillingSession struct {
	mu             sync.Mutex
	userID         int
	tokenID        int
	tokenUnlimited bool
	preConsumed    int
	state          BillingState
}

// NewBillingSession 创建一个处于 init 状态的计费会话。
func NewBillingSession(userID, tokenID int, tokenUnlimited bool) *BillingSession {
	return &BillingSession{
		userID:         userID,
		tokenID:        tokenID,
		tokenUnlimited: tokenUnlimited,
		state:          BillingInit,
	}
}

// PreConsume 原子地冻结 quota，并把会话推进到 preConsumed 状态。
//
// 只有 init 状态可以预扣；第二次调用是编程错误（同一请求只该预扣一次），
// 因此返回 error 而不是静默成功——静默成功会让调用方误以为又扣了一笔。
//
// quota <= 0 表示免费模型：不碰数据库，但状态仍推进到 preConsumed。
// 免费模型也必须走完同一套状态机，这样结算/退款路径不需要为"没预扣过"
// 单独开分支，免费与收费请求的外部行为保持一致。
//
// 返回实际预扣的额度。额度不足与数据库错误都会让会话停留在 init：
// 预扣失败时没有任何金额被冻结，调用方可以安全地不再结算/退款。
func (s *BillingSession) PreConsume(quota int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != BillingInit {
		return 0, fmt.Errorf("非法计费状态转移：%s 不能再次预扣（只允许 init → preConsumed）", s.state)
	}

	if quota <= 0 {
		s.preConsumed = 0
		s.state = BillingPreConsumed
		return 0, nil
	}

	ok, err := model.ReserveQuota(s.userID, s.tokenID, s.tokenUnlimited, quota)
	if err != nil {
		// 会话保持 init：事务已回滚，没有任何余额被冻结。
		return 0, fmt.Errorf("预扣额度失败: %w", err)
	}
	if !ok {
		// 余额不足不是数据库故障；用哨兵错误让调用方能 errors.Is 后映射成 quota 错误。
		return 0, fmt.Errorf("预扣 %d quota 失败: %w", quota, ErrInsufficientQuota)
	}

	s.preConsumed = quota
	s.state = BillingPreConsumed
	return quota, nil
}

// Settle 按真实用量 actual 结算预扣差额，并把实际消耗累加进 used_quota。
//
// 只有 preConsumed 状态才执行；其余状态返回 (false, nil) 而不是 error。
// 原因：Settle 通常挂在请求结束的 defer 上，而 defer 可能因为重试、
// 提前 return 或多层封装被执行多次。重复调用是**预期内**的正常情况，
// 返回 false 表示"这次没有做任何事"，调用方无需把正常的幂等调用当故障处理；
// 只有真正的数据库错误才返回 error。
//
// actual < 0 是不可信的输入（上游 usage 解析异常等），按 0 处理：
// 负用量会让结算变成"退款"并污染 used_quota。这里只做防御性钳制，
// 正常路径的计价值在 CalculateTextQuota 里已经保证非负。
//
// 数据库出错时会话保持 preConsumed，调用方仍可重试结算或改走退款；
// 状态绝不能先于账本更新成功而推进。
func (s *BillingSession) Settle(actual int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != BillingPreConsumed {
		return false, nil
	}

	if actual < 0 {
		actual = 0
	}

	// 差额语义由 model.SettleQuota 定义：delta>0 补扣（允许负余额），
	// delta<0 退回差额，delta==0 只累加 used_quota。
	delta := actual - s.preConsumed
	if err := model.SettleQuota(s.userID, s.tokenID, s.tokenUnlimited, delta, actual); err != nil {
		return false, fmt.Errorf("结算失败: %w", err)
	}

	s.state = BillingSettled
	return true, nil
}

// Refund 把预扣金额原样退回用户钱包与令牌额度。
//
// 与 Settle 一样，只有 preConsumed 状态才执行；其他状态返回 (false, nil)。
// 退款同样由 defer 触发，且典型调用顺序是"先 defer Refund、成功时再 Settle"：
// 当 Settle 已经抢先执行后，defer 里的 Refund 必须是无害的 no-op，
// 不能因为"已经结算过"就把正常的收尾变成错误。
//
// 数据库出错时会话保持 preConsumed，状态与账本仍然一致。
func (s *BillingSession) Refund() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != BillingPreConsumed {
		return false, nil
	}

	if err := model.RefundQuota(s.userID, s.tokenID, s.tokenUnlimited, s.preConsumed); err != nil {
		return false, fmt.Errorf("退款失败: %w", err)
	}

	s.state = BillingRefunded
	return true, nil
}

// State 返回当前状态；加锁是为了让并发测试/日志读取不会与状态迁移竞争。
func (s *BillingSession) State() BillingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// PreConsumed 返回实际冻结的额度；未预扣或免费模型时为 0。
func (s *BillingSession) PreConsumed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preConsumed
}
