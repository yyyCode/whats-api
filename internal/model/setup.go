package model

import (
	"fmt"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
)

// Setup 记录"这个库被首次初始化过"。
type Setup struct {
	ID            int    `gorm:"primaryKey"`
	Version       string `gorm:"size:32"`
	InitializedAt int64
}

// TableName 固定为单数：这是安装标志表，不跟 GORM 的复数化规则走。
func (Setup) TableName() string { return "setup" }

// CheckSetup 保证 setup 表里有且只有 ID=1 的那一行。
//
// 幂等：已存在就不动。只在 master 执行，与迁移同一条纪律——
// 多实例同时插第一行会撞主键，而第二个实例报错没有任何收益。
func CheckSetup() error {
	if !common.IsMasterNode {
		return nil
	}

	done, err := SetupComplete()
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	row := Setup{ID: 1, Version: common.Version, InitializedAt: time.Now().Unix()}
	if err := DB.Create(&row).Error; err != nil {
		return fmt.Errorf("写入 setup 首行失败: %w", err)
	}
	logger.Info("首次安装：已写入 setup 记录", "version", row.Version)
	return nil
}

// SetupComplete 报告是否已经初始化过（供 /api/status）。
func SetupComplete() (bool, error) {
	var count int64
	if err := DB.Model(&Setup{}).Count(&count).Error; err != nil {
		return false, fmt.Errorf("读取 setup 表失败: %w", err)
	}
	return count > 0, nil
}
