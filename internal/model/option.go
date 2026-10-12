package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm/clause"

	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/setting/config"
	"github.com/yyyCode/whats-api/internal/setting/system_setting"
)

// Option 是运行时配置的持久化形态：key → JSON 编码的值。
//
// 表里只存"管理员显式改过"的项，默认值来自代码里的注册结构体
// （见 internal/setting/config）。这样将来调默认值能影响所有未显式修改的实例。
type Option struct {
	// key 是三库的关键字，列名必须显式指定；GORM 生成的 DDL 会给它加引号。
	Key   string `gorm:"column:key;primaryKey;size:128"`
	Value string `gorm:"type:text"`
}

// OptionMap 是内存中的配置镜像：key → JSON 编码的值。
//
// 它存在的意义是"比较基准"：SyncOptions 每轮把库里的行与它对比，
// 只对真的变了的项走一遍应用流程（否则每轮都在重建快照、刷日志）。
//
// 它是进程级可变状态，但只有 updateOptionMap 一个写入点。
var OptionMap = map[string]string{}

// InitOptionMap 装载配置：先用注册表里的默认值打底，再用库里的行覆盖。
//
// ★ 必须在任何读配置的模块之前调用（设计文档 §3 的第 4 步）——
// 包括 InitLogDB（日志库 DSN 将来可能来自配置）。
func InitOptionMap() error {
	OptionMap = config.GlobalConfig.ExportAllConfigs()

	rows, err := loadOptionsFromDatabase()
	if err != nil {
		return err
	}

	for key, value := range rows {
		if err := updateOptionMap(key, value); err != nil {
			// 库里的脏数据（比如降级安装残留的 key）不该让服务起不来。
			// 跳过并告警，等管理员来修——注意此时 OptionMap 保持默认值。
			logger.Warn("忽略无法应用的配置项", "key", key, "value", value, "error", err)
		}
	}

	logger.Info("配置已装载", "keys", len(OptionMap), "from_db", len(rows))
	return nil
}

// UpdateOption 是配置的**唯一写入路径**。
//
// 顺序不能颠倒：校验 → 落库 → 改内存。
// 先改内存再落库的话，落库失败会留下"这个实例和库不一致"的状态，
// 重启后表现为"配置莫名其妙回退了"，而且排查时看不到任何报错。
func UpdateOption(key, value string) error {
	// 校验在落库之前完成：库里的脏值会被别的实例同步过去，一处错变成处处错。
	if err := config.GlobalConfig.ValidateConfig(key, value); err != nil {
		return err
	}
	if err := saveOption(key, value); err != nil {
		return err
	}
	if err := updateOptionMap(key, value); err != nil {
		return err
	}
	logger.Info("配置已更新", "key", key)
	return nil
}

// updateOptionMap 是唯一的配置分发点：内存 map、运行时结构体、副作用三者一起改。
//
// 分成三件事的原因：
//   - OptionMap 是同步比较基准；
//   - UpdateConfig 写注册结构体并触发 OnRebuild，从而**原子发布**该域的快照；
//   - applySideEffects 处理没有对应结构体字段的开关（M0 只有日志级别）。
func updateOptionMap(key, value string) error {
	if err := config.GlobalConfig.UpdateConfig(key, value); err != nil {
		return err
	}
	OptionMap[key] = value
	applySideEffects(key)
	return nil
}

// applySideEffects 处理"非结构化副作用"，集中在这一个 switch 里。
//
// M0 只有日志级别需要它：其余配置项改完由读快照的代码（中间件、/api/status）自动生效，
// 不需要在这里加任何东西——每加一条都是一个新的隐式耦合点。
func applySideEffects(key string) {
	switch key {
	case "system_setting.log_level":
		level := system_setting.Get().LogLevel
		logger.SetLevel(logger.ParseLevel(level))
		logger.Info("日志级别已更新", "log_level", level)
	}
}

// saveOption 把单个配置项 upsert 进库。
//
// clause.OnConflict 让 GORM 按方言生成 MySQL 的 ON DUPLICATE KEY UPDATE
// 或 PostgreSQL / SQLite 的 ON CONFLICT DO UPDATE——一份代码，三库通用。
func saveOption(key, value string) error {
	row := Option{Key: key, Value: value}
	err := DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("保存配置 %q 失败: %w", key, err)
	}
	return nil
}

func loadOptionsFromDatabase() (map[string]string, error) {
	// DB 为 nil 必须返回错误而不是解引用：CloseDB 会把句柄置 nil，
	// 而 SyncOptions 是后台 goroutine，停机途中完全可能正好走到一轮。
	// 那时 panic 会把整个进程掀掉，而这只是一次"库已经关了"的普通失败。
	// Migrate/migrateLOGDB 对同一个前置条件也是这样显式报错的。
	if DB == nil {
		return nil, errors.New("主库未初始化，无法读取配置")
	}

	var rows []Option
	if err := DB.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取 options 表失败: %w", err)
	}

	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// SyncOptions 定时把库里的配置重新装载一遍，直到 ctx 被取消。
//
// **全量重载**而不是增量：某实例错过一次变更（网络抖动、进程重启）后，
// 下一轮就能自己追上，不需要任何"补偿"逻辑。
// 但只对值真的变了的项走应用流程，避免无谓的快照重建与日志噪声。
func SyncOptions(ctx context.Context, freq time.Duration) {
	ticker := time.NewTicker(freq)
	defer ticker.Stop()

	logger.Info("配置定时同步已启动", "interval_seconds", int(freq.Seconds()))
	for {
		select {
		case <-ctx.Done():
			logger.Info("配置定时同步已停止")
			return
		case <-ticker.C:
			applied, err := syncOptionsOnce()
			if err != nil {
				// 一轮失败不是事故：下一轮会重试，全量重载保证最终收敛。
				logger.Warn("配置同步失败，下一轮重试", "error", err)
				continue
			}
			if applied > 0 {
				logger.Info("配置同步完成", "applied", applied)
			}
		}
	}
}

// syncOptionsOnce 执行一轮同步，返回本轮实际应用的项数（测试据此断言"没变就不应用"）。
func syncOptionsOnce() (int, error) {
	pending, err := loadOptionsFromDatabase()
	if err != nil {
		return 0, err
	}

	applied := 0
	for key, value := range pending {
		if OptionMap[key] == value {
			continue
		}
		if err := updateOptionMap(key, value); err != nil {
			logger.Warn("同步配置项失败，跳过", "key", key, "error", err)
			continue
		}
		applied++
	}
	return applied, nil
}
