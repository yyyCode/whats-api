// Package model 是数据层：GORM 模型、连接管理、迁移、以及配置的持久化读写。
//
// 本项目要求同一份代码跑在 SQLite / MySQL / PostgreSQL 上，所以这里有两条纪律：
//  1. 不写数据库特有的语法，方言差异一律收进本文件的 helper；
//  2. 迁移必须幂等——连续启动三次不能有任何副作用。
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
)

// DB 是主库；LOG_DB 是日志库（未单独配置时与 DB 是同一个连接）。
//
// 它们是"进程级单例句柄"：Init* 写一次，之后只读。测试通过重新 Init
// 指向临时库来注入——除此之外没有任何地方会写它们。
var (
	DB     *gorm.DB
	LOG_DB *gorm.DB
)

type dbKind string

const (
	dbKindSQLite   dbKind = "sqlite"
	dbKindMySQL    dbKind = "mysql"
	dbKindPostgres dbKind = "postgres"
)

// 连接池参数（仅用于非 SQLite：SQLite 是单写者，见 openDB）。
const (
	maxOpenConns    = 100
	maxIdleConns    = 20
	connMaxLifetime = time.Hour
	connMaxIdleTime = 10 * time.Minute
)

// InitDB 初始化主库并执行迁移。★这是唯一的致命初始化：
// 主库连不上就没有任何业务可谈，直接让启动失败（其余初始化失败都降级）。
func InitDB() error {
	dialector, kind, err := chooseDB(common.SQLDSN, false)
	if err != nil {
		return fmt.Errorf("主库 DSN 无效: %w", err)
	}

	db, err := openDB(dialector, kind)
	if err != nil {
		return fmt.Errorf("连接主库失败: %w", err)
	}

	// 先摘掉旧句柄再装新的：重复 Init（测试重入、将来的热重载）时，
	// 上一个连接池会被直接覆盖成孤儿，SQLite 还会一直攥着文件句柄不放。
	closePool(DB)
	DB = db

	if err := Migrate(); err != nil {
		return err
	}
	return nil
}

// InitLogDB 初始化日志库。调用点必须在 InitOptionMap 之后
// （日志库 DSN 将来可能来自配置项），并且 LOG_SQL_DSN 为空时复用主库连接。
func InitLogDB() error {
	if common.LogSQLDSN == "" || common.LogSQLDSN == common.SQLDSN {
		// 旧日志句柄只有独立于主库时才关：与主库同一句柄时关掉就等于关了主库。
		if LOG_DB != DB {
			closePool(LOG_DB)
		}
		LOG_DB = DB
		logger.Info("未单独配置日志库，复用主库")
	} else {
		dialector, kind, err := chooseDB(common.LogSQLDSN, true)
		if err != nil {
			return fmt.Errorf("日志库 DSN 无效: %w", err)
		}
		logDB, err := openDB(dialector, kind)
		if err != nil {
			return fmt.Errorf("连接日志库失败: %w", err)
		}
		if LOG_DB != DB {
			closePool(LOG_DB)
		}
		LOG_DB = logDB
		logger.Info("日志库已独立配置", "driver", LogDriver())
	}

	return migrateLOGDB()
}

// chooseDB 按 DSN 前缀选择驱动。返回 dialector 而不是连接：
// 连接由调用方建立，测试可以直接断言"分派对了"而不需要真实数据库。
func chooseDB(dsn string, isLog bool) (gorm.Dialector, dbKind, error) {
	switch {
	case dsn == "" || dsn == "local" || strings.HasPrefix(dsn, "local:"):
		return sqlite.Open(sqliteFile(dsn)), dbKindSQLite, nil

	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		// PreferSimpleProtocol 让驱动走简单查询协议，兼容 PgBouncer / Neon 这类连接池。
		return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true}), dbKindPostgres, nil

	case isClickHouseDSN(dsn):
		// 分支留好但明确不实现——比"假装支持然后连不上"诚实，
		// 也让配错 DSN 的人在启动日志里立刻看到原因。
		if !isLog {
			// 不回显 dsn：这是启动路径的错误，会原样进日志，而 DSN 里通常有密码。
			return nil, "", errors.New("主库不支持 ClickHouse（该 DSN 形式只允许用于日志库）")
		}
		return nil, "", errors.New("ClickHouse 日志库尚未引入（计划在 M6）")

	default:
		return mysql.Open(ensureParseTime(dsn)), dbKindMySQL, nil
	}
}

// isClickHouseDSN 识别 ClickHouse 的几种 DSN 写法（驱动尚未引入）。
func isClickHouseDSN(dsn string) bool {
	for _, prefix := range []string{"clickhouse://", "tcp://", "http://", "https://"} {
		if strings.HasPrefix(dsn, prefix) {
			return true
		}
	}
	return false
}

// sqliteFile 把 DSN 解析成 SQLite 文件路径。
func sqliteFile(dsn string) string {
	switch {
	case dsn == "", dsn == "local", dsn == "local:":
		return common.DefaultSQLiteFile
	default:
		if path := strings.TrimSpace(strings.TrimPrefix(dsn, "local:")); path != "" {
			return path
		}
		return common.DefaultSQLiteFile
	}
}

// ensureParseTime 给 MySQL DSN 补 parseTime=true。
// 缺了它，DATETIME/TIMESTAMP 列会以 []byte 返回，扫进 time 类型时直接报错。
func ensureParseTime(dsn string) string {
	if strings.Contains(dsn, "parseTime=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "parseTime=true"
}

func openDB(dialector gorm.Dialector, kind dbKind) (*gorm.DB, error) {
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormLogger(),
		// 不让 AutoMigrate 建外键约束：三库对 ON DELETE 行为不一致，
		// 而本项目的一致性不靠数据库约束保证（日志与账本更是只追加）。
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}

	if kind == dbKindSQLite {
		// SQLite 是单写者：连接数放开只会制造 "database is locked"。
		// 限制为 1 之后，写操作在 Go 侧排队，比在 SQLite 侧互相报错好。
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		if err := applySQLitePragmas(db); err != nil {
			return nil, err
		}
	} else {
		sqlDB.SetMaxOpenConns(maxOpenConns)
		sqlDB.SetMaxIdleConns(maxIdleConns)
		sqlDB.SetConnMaxLifetime(connMaxLifetime)
		sqlDB.SetConnMaxIdleTime(connMaxIdleTime)
	}

	return db, nil
}

// applySQLitePragmas 设置 SQLite 的行为。三条都不是可选项：
//   - WAL：读不阻塞写。多实例共用一个文件时，没有 WAL 就是随机 "database is locked"；
//   - busy_timeout：遇到写锁先等 5 秒再报错，而不是立刻失败（多实例的抖动容忍）；
//   - foreign_keys：SQLite 默认**关闭**外键约束，与 MySQL/PG 行为不一致。
func applySQLitePragmas(db *gorm.DB) error {
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if err := db.Exec(pragma).Error; err != nil {
			return fmt.Errorf("执行 %s 失败: %w", pragma, err)
		}
	}
	return nil
}

func gormLogger() gormlogger.Interface {
	if common.GinMode == "debug" {
		return gormlogger.Default.LogMode(gormlogger.Info)
	}
	// 生产模式只记慢查询与错误：把每条 SQL 刷进日志会把有用的行淹掉。
	return gormlogger.Default.LogMode(gormlogger.Warn)
}

// closePool 关掉一个旧句柄背后的连接池（若有），只为"即将用新句柄覆盖它"的场景。
// 纪律同 InitRedisClient：装新句柄之前先把旧的摘掉并关停，
// 否则第二次 Init 会把上一个连接池覆盖成孤儿，句柄与连接永不释放。
func closePool(db *gorm.DB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// CloseDB 关闭主库与日志库（同一个连接时只关一次）。
// 停机时序里它必须是最后一步：早于后台 goroutine 退出就会让它们写到已关闭的连接上。
func CloseDB() error {
	var errs []error

	if LOG_DB != nil && LOG_DB != DB {
		if sqlDB, err := LOG_DB.DB(); err == nil {
			errs = append(errs, sqlDB.Close())
		}
	}
	if DB != nil {
		if sqlDB, err := DB.DB(); err == nil {
			errs = append(errs, sqlDB.Close())
		}
	}

	DB, LOG_DB = nil, nil
	return errors.Join(errs...)
}

// ---------- 方言 helper ----------
//
// 这些函数在 M0 只有测试调用（M0 没有裸 SQL，GORM 自己会给标识符加引号）。
// 它们存在的意义是：M1/M2 写条件 UPDATE 与裸 SQL 时不必回头改数据层约定。
// 实现为函数而不是包级变量——方言要等 InitDB 之后才知道，
// 做成变量会在初始化早期被读到空串，静默出错。

func dialectName() string {
	if DB == nil {
		return ""
	}
	return DB.Dialector.Name()
}

func isSQLite() bool   { return dialectName() == string(dbKindSQLite) }
func isPostgres() bool { return dialectName() == string(dbKindPostgres) }
func isMySQL() bool    { return dialectName() == string(dbKindMySQL) }

// commonKeyCol 返回保留字列 key 的引用写法（tokens.key、channels.key、options.key）。
// 三库里 key 都是关键字，裸写会报语法错误。
func commonKeyCol() string { return keyColFor(dialectName()) }

func keyColFor(dialect string) string {
	if dialect == string(dbKindPostgres) {
		return `"key"`
	}
	return "`key`" // MySQL / SQLite
}

// commonTrueVal / commonFalseVal 返回当前方言的布尔字面量。
// 裸 SQL 里写 1/0 在 MySQL 能用，在 PostgreSQL 的 boolean 列上会报类型错误。
func commonTrueVal() string  { return trueValFor(dialectName()) }
func commonFalseVal() string { return falseValFor(dialectName()) }

func trueValFor(dialect string) string {
	if dialect == string(dbKindPostgres) {
		return "true"
	}
	return "1"
}

func falseValFor(dialect string) string {
	if dialect == string(dbKindPostgres) {
		return "false"
	}
	return "0"
}

// lockForUpdate 给查询加行锁（M2 的并发预扣会用到）。
//
// ★ 绝不要写 tx.Set("gorm:query_option", "FOR UPDATE")：GORM v2 会**静默忽略**它，
// 于是代码看起来加了锁、SQL 里却没有，这是并发保护里最阴险的一类 bug。
func lockForUpdate(tx *gorm.DB) *gorm.DB { return lockForUpdateFor(dialectName(), tx) }

func lockForUpdateFor(dialect string, tx *gorm.DB) *gorm.DB {
	if dialect == string(dbKindSQLite) {
		// SQLite 是单写者 + 库级写锁，没有行锁语法；原样返回而不是报错。
		return tx
	}
	return tx.Clauses(clause.Locking{Strength: "UPDATE"})
}

// ---------- 状态查询（供 /api/status）----------

func MainDriver() string {
	if DB == nil {
		return ""
	}
	return DB.Dialector.Name()
}

func LogDriver() string {
	if LOG_DB == nil {
		return ""
	}
	return LOG_DB.Dialector.Name()
}

// LogDBSeparate 报告日志库是否独立于主库。
func LogDBSeparate() bool { return LOG_DB != nil && LOG_DB != DB }

// PingMain / PingLog 供 /api/status 的 ok 字段。带超时：
// 探活接口不能被一个卡住的连接拖住。
func PingMain() bool { return ping(DB) }
func PingLog() bool  { return ping(LOG_DB) }

func ping(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	sqlDB, err := db.DB()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return sqlDB.PingContext(ctx) == nil
}

// DSNKind 描述连接去向，用于 /api/status 展示。
// 只回一个枚举值，不回显 DSN 全文——DSN 里有密码。
func DSNKind() string {
	dsn := common.SQLDSN
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "server"
	case dsn != "" && !strings.HasPrefix(dsn, "local"):
		return "server" // MySQL
	case strings.Contains(dsn, ":memory:"):
		return "memory"
	default:
		return "file"
	}
}

// TODO(Task 2): 这一节是让 Task 1 的测试能编译的临时桩，Task 2 会被真实实现替换。
var OptionMap = map[string]string{}

func Migrate() error      { return nil }
func migrateLOGDB() error { return nil }

type Option struct {
	Key   string `gorm:"column:key;primaryKey;size:128"`
	Value string `gorm:"type:text"`
}
