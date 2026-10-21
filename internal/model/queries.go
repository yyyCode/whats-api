package model

import (
	"errors"

	"gorm.io/gorm"

	"github.com/yyyCode/whats-api/internal/constant"
)

// ErrNotFound 是"查了但没有"的统一哨兵。
// 调用方必须能区分它与数据库故障：前者是 401，后者是 500。
var ErrNotFound = errors.New("记录不存在")

// db 返回主库句柄。每个查询都先过这一道：
// 初始化早期的 nil 句柄应该给出明确错误，而不是在 GORM 内部空指针崩溃。
func db() (*gorm.DB, error) {
	if DB == nil {
		return nil, errors.New("主库未初始化")
	}
	return DB, nil
}

// GetTokenByKey 按明文 key 查令牌。M1 直接查库（key 上有唯一索引），缓存是 M3 的事。
func GetTokenByKey(key string) (*Token, error) {
	d, err := db()
	if err != nil {
		return nil, err
	}
	var tok Token
	// key 是 MySQL/PG/SQLite 的保留字，必须经方言 helper 加引号。
	if err := d.Where(commonKeyCol()+" = ?", key).First(&tok).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &tok, nil
}

// GetUserByID 查令牌所属用户。
func GetUserByID(id int) (*User, error) {
	d, err := db()
	if err != nil {
		return nil, err
	}
	var u User
	if err := d.First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// GetEnabledChannels 取全部启用中的渠道。选路的过滤（分组/模型/排除）在 service 层做，
// 这样"查什么"与"怎么选"分开，选路逻辑可以脱离数据库单测。
func GetEnabledChannels() ([]Channel, error) {
	d, err := db()
	if err != nil {
		return nil, err
	}
	var channels []Channel
	if err := d.Where("status = ?", constant.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}
	return channels, nil
}

// DisableChannel 把启用中的渠道置为自动禁用。
//
// 条件里必须带 status = enabled：手动禁用的渠道不能被自动化改写，
// 否则 M4 的"只恢复自动禁用的"就失去了判断依据。返回是否真的改写了。
func DisableChannel(id int) (bool, error) {
	d, err := db()
	if err != nil {
		return false, err
	}
	res := d.Model(&Channel{}).
		Where("id = ? AND status = ?", id, constant.ChannelStatusEnabled).
		Update("status", constant.ChannelStatusAutoDisabled)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// TouchTokenAccessed 记录令牌最近使用时间。
// 鉴权中间件按分钟节流调用它——SQLite 是单写者，每个请求写一次会把并发串行化。
func TouchTokenAccessed(id int, ts int64) error {
	d, err := db()
	if err != nil {
		return err
	}
	return d.Model(&Token{}).Where("id = ?", id).Update("accessed_at", ts).Error
}
