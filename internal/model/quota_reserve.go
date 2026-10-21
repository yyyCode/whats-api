package model

// 本文件实现账本的三类原子操作：预扣、结算、退款。
//
// ★ 并发不变量：余额永远不会被透支，完全依赖条件 UPDATE 的 WHERE 子句：
//
//	UPDATE users SET quota = quota - ? WHERE id = ? AND quota >= ?
//
// 数据库在执行这条语句时会锁住匹配行，"判断够不够"与"减法"发生在同一条
// 语句里，两者之间没有窗口。先 SELECT 余额、再在 Go 里判断、最后 UPDATE
// 是错的：两个并发请求可以同时读到 100，各自认为够扣 100，最终余额变成 -100。
// 同理，Read-Modify-Write 形式（SELECT 出来加加减减再写回）也必须避免。
//
// 所有写入都用 UpdateColumn + gorm.Expr，不经过 GORM 的模型值回写，
// 也不写任何数据库特有语法。

import (
	"errors"
	"fmt"
	"math"

	"gorm.io/gorm"

	"github.com/yyyCode/whats-api/internal/common"
)

// errQuotaInsufficient 只在事务回调内部用于触发回滚。
//
// GORM 的 Transaction 只有在回调返回非 nil error 时才回滚；如果直接
// return (false, nil) 式的 nil error，事务会把"令牌已扣、用户没扣"的
// 中间状态提交掉。因此组合函数用这个哨兵错误强制回滚，再在外层翻译成
// (false, nil)——对调用方而言仍然只是"额度不足"。
var errQuotaInsufficient = errors.New("额度不足")

// TryReserveUserQuota 原子地从用户钱包预扣 quota。
//
// 并发保护就是下面 UPDATE 的 `quota >= ?` 条件（见文件头注释）：
// SELECT-then-UPDATE 会在这两步之间把同一条余额同时给到多个请求，造成透支。
// RowsAffected == 1 才表示条件成立且确实扣到了；0 表示余额不足或用户不存在。
func TryReserveUserQuota(id, quota int) (bool, error) {
	if quota <= 0 {
		// 无需冻结任何金额，也就无需碰数据库。
		return true, nil
	}
	d, err := db()
	if err != nil {
		return false, err
	}
	return tryReserveUserQuota(d, id, quota)
}

// tryReserveUserQuota 是 TryReserveUserQuota 的事务内版本。
// 它只接受非 nil 句柄：外层 ReserveQuota 已经通过 db() 做过空句柄检查。
func tryReserveUserQuota(d *gorm.DB, id, quota int) (bool, error) {
	res := d.Model(&User{}).
		Where("id = ? AND quota >= ?", id, quota).
		UpdateColumn("quota", gorm.Expr("quota - ?", quota))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// TryReserveTokenQuota 原子地从令牌额度预扣 quota。
// unlimited 令牌忽略 remain_quota，视为永远够用；quota<=0 同样是无操作。
//
// 并发保护同样只来自 `remain_quota >= ?` 这个条件：
// 令牌是比用户更具体的限额，先扣它，余额不足时整个请求在预扣阶段就该失败。
func TryReserveTokenQuota(id, quota int, unlimited bool) (bool, error) {
	if unlimited || quota <= 0 {
		return true, nil
	}
	d, err := db()
	if err != nil {
		return false, err
	}
	return tryReserveTokenQuota(d, id, quota, unlimited)
}

// tryReserveTokenQuota 是 TryReserveTokenQuota 的事务内版本。
func tryReserveTokenQuota(d *gorm.DB, id, quota int, unlimited bool) (bool, error) {
	if unlimited || quota <= 0 {
		return true, nil
	}
	res := d.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", id, quota).
		UpdateColumn("remain_quota", gorm.Expr("remain_quota - ?", quota))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ReserveQuota 在同一个事务里先扣令牌、再扣用户；任一失败则整体回滚。
//
// 顺序固定为"先令牌、后用户"：令牌是更具体的限额，而且所有预扣事务都按
// 相同顺序拿行锁，减少并发下互相等待的机会。
//
// 任一条件 UPDATE 影响 0 行时回滚整个事务，返回 (false, nil)——
// "余额不足"不是数据库错误，调用方据此返回 403/quota 错误即可。
func ReserveQuota(userID, tokenID int, unlimited bool, quota int) (bool, error) {
	d, err := db()
	if err != nil {
		return false, err
	}

	reserved := false
	err = d.Transaction(func(tx *gorm.DB) error {
		ok, err := tryReserveTokenQuota(tx, tokenID, quota, unlimited)
		if err != nil {
			return err
		}
		if !ok {
			return errQuotaInsufficient
		}

		ok, err = tryReserveUserQuota(tx, userID, quota)
		if err != nil {
			return err
		}
		if !ok {
			return errQuotaInsufficient
		}

		reserved = true
		return nil
	})
	if errors.Is(err, errQuotaInsufficient) {
		// 哨兵错误只用于让 GORM 回滚；对调用方等价于"额度不足"。
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return reserved, nil
}

// SettleQuota 按 delta 调整预扣差额，并把真实用量 actual 累加进 used_quota。
//
// delta > 0：补扣（quota -= delta），无条件执行——上游已经产生费用，
// 这时拒绝补扣等于白送，因此允许余额变负。
// delta < 0：退回差额（quota += -delta），但只在加回后不会越过钱包上限时执行，
// 否则整数溢出会把余额卷成负数。条件不成立时返回错误，整个事务回滚。
// delta == 0：不再改动 quota，只累加 used_quota。
//
// used_quota 只在结算时按 actual 累加，预扣与退款都不碰它，
// 这样账本上的"累计消耗"永远等于成功请求的真实花费。
//
// 事务内先处理令牌再处理用户，与 ReserveQuota 保持同一把锁的获取顺序。
func SettleQuota(userID, tokenID int, unlimited bool, delta, actual int) error {
	d, err := db()
	if err != nil {
		return err
	}

	return d.Transaction(func(tx *gorm.DB) error {
		if !unlimited {
			if err := settleTokenQuota(tx, tokenID, delta, actual); err != nil {
				return err
			}
		}
		return settleUserQuota(tx, userID, delta, actual)
	})
}

// settleUserQuota 在事务内结算用户钱包。
func settleUserQuota(tx *gorm.DB, id, delta, actual int) error {
	switch {
	case delta > 0:
		// 补扣不设下限：允许余额为负，但必须真的命中了这一行。
		res := tx.Model(&User{}).
			Where("id = ?", id).
			UpdateColumn("quota", gorm.Expr("quota - ?", delta))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("结算用户 %d 失败：用户不存在", id)
		}

	case delta < 0:
		// refund 是正数；只有 quota <= MaxWalletQuota-refund 时相加才不溢出。
		// 条件放在 WHERE 里与加法同语句执行，避免判断与写入之间的竞态。
		refund := -delta
		res := tx.Model(&User{}).
			Where("id = ? AND quota <= ?", id, int64(common.MaxWalletQuota)-int64(refund)).
			UpdateColumn("quota", gorm.Expr("quota + ?", refund))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("退回用户 %d 差额 %d 失败：用户不存在或余额将超过上限 %d",
				id, refund, common.MaxWalletQuota)
		}
	}

	// 不放宽上限，直接累加；actual 为 0 时语句仍是合法 no-op。
	return tx.Model(&User{}).
		Where("id = ?", id).
		UpdateColumn("used_quota", gorm.Expr("used_quota + ?", actual)).Error
}

// settleTokenQuota 在事务内结算令牌额度。remain_quota 是 int64，
// 退款上限用 math.MaxInt64 而不是 MaxWalletQuota。
func settleTokenQuota(tx *gorm.DB, id, delta, actual int) error {
	switch {
	case delta > 0:
		res := tx.Model(&Token{}).
			Where("id = ?", id).
			UpdateColumn("remain_quota", gorm.Expr("remain_quota - ?", delta))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("结算令牌 %d 失败：令牌不存在", id)
		}

	case delta < 0:
		refund := int64(-delta)
		res := tx.Model(&Token{}).
			Where("id = ? AND remain_quota <= ?", id, math.MaxInt64-refund).
			UpdateColumn("remain_quota", gorm.Expr("remain_quota + ?", refund))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("退回令牌 %d 差额 %d 失败：令牌不存在或余额将超过上限", id, refund)
		}
	}

	return tx.Model(&Token{}).
		Where("id = ?", id).
		UpdateColumn("used_quota", gorm.Expr("used_quota + ?", actual)).Error
}

// RefundQuota 把预扣金额原样加回用户钱包与令牌额度（unlimited 令牌跳过）。
// 退款不修改 used_quota：请求没有成功计费，累计消耗就不该变化。
//
// 与 SettleQuota 一样，加回操作带溢出上限条件；条件不成立时返回错误并回滚。
func RefundQuota(userID, tokenID int, unlimited bool, amount int) error {
	if amount <= 0 {
		return nil
	}
	d, err := db()
	if err != nil {
		return err
	}

	return d.Transaction(func(tx *gorm.DB) error {
		// 与 Reserve/Settle 保持先令牌、后用户的顺序。
		if !unlimited {
			if err := refundTokenQuota(tx, tokenID, amount); err != nil {
				return err
			}
		}
		return refundUserQuota(tx, userID, amount)
	})
}

// refundUserQuota 在事务内把 amount 加回用户钱包，不碰 used_quota。
func refundUserQuota(tx *gorm.DB, id, amount int) error {
	res := tx.Model(&User{}).
		Where("id = ? AND quota <= ?", id, int64(common.MaxWalletQuota)-int64(amount)).
		UpdateColumn("quota", gorm.Expr("quota + ?", amount))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("退款失败：用户 %d 不存在或余额将超过上限 %d", id, common.MaxWalletQuota)
	}
	return nil
}

// refundTokenQuota 在事务内把 amount 加回令牌额度，不碰 used_quota。
func refundTokenQuota(tx *gorm.DB, id, amount int) error {
	refund := int64(amount)
	res := tx.Model(&Token{}).
		Where("id = ? AND remain_quota <= ?", id, math.MaxInt64-refund).
		UpdateColumn("remain_quota", gorm.Expr("remain_quota + ?", refund))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("退款失败：令牌 %d 不存在或余额将超过上限", id)
	}
	return nil
}
