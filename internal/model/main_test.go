package model

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yyyCode/whats-api/internal/common"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// restoreEnvGlobals 让本包测试不受 common 包级全局残留的影响。
//
// t.Setenv 只还原进程环境变量，**不会**还原 common.InitEnv() 写下的包级全局
// （IsMasterNode / SQLDSN / NodeType …）。一个设了 NODE_TYPE=slave 的测试
// 会把 IsMasterNode 留成 false，而 CheckSetup/Migrate 在非 master 下静默 no-op——
// 于是后续测试的失败取决于执行顺序，很难看出来。
//
// ★ 必须在任何 t.Setenv 之前调用：t.Cleanup 是 LIFO，先注册的这个回调因此最后执行，
// 那时环境变量已被 t.Setenv 还原，再跑一次 InitEnv 就把全局重新对齐到进程初始状态。
func restoreEnvGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(common.InitEnv)
}

// withTestDB 用临时目录里的真实 SQLite 文件建立主库与日志库，
// 而不是内存库：迁移幂等、WAL、多进程共享这些行为都只有真实文件才测得到。
//
// ★ 每个测试最多调用一次。它写的是包级 DB/LOG_DB 单例句柄，
// 在循环里反复调用会让上一轮的连接池变成孤儿（Cleanup 的 LIFO 顺序只关得掉最后一个），
// 于是 t.TempDir 的清理在 Windows 上直接报 "being used by another process"。
func withTestDB(t *testing.T) {
	t.Helper()
	restoreEnvGlobals(t)

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "test.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	common.InitEnv()

	t.Cleanup(func() {
		_ = CloseDB()
		OptionMap = map[string]string{}
	})

	if err := InitDB(); err != nil {
		t.Fatalf("InitDB 失败: %v", err)
	}
	if err := InitLogDB(); err != nil {
		t.Fatalf("InitLogDB 失败: %v", err)
	}
}

// 驱动分派：DSN 前缀决定驱动，空/ local 走 SQLite。
// 本机没有 MySQL/PG 服务，所以这里只断言"分派对了"，
// 不谎称连过真实库（真实连接需在有用例的环境复核）。
func TestChooseDBDispatch(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		isLog   bool
		want    string // 空表示期望报错
		wantErr string // 错误信息需包含的片段
	}{
		{name: "空 DSN 走 SQLite", dsn: "", want: "sqlite"},
		{name: "local 走 SQLite", dsn: "local", want: "sqlite"},
		{name: "local:路径 走 SQLite", dsn: "local:/tmp/x.db", want: "sqlite"},
		{name: "postgres:// 走 PostgreSQL", dsn: "postgres://u:p@localhost:5432/db", want: "postgres"},
		{name: "postgresql:// 走 PostgreSQL", dsn: "postgresql://u:p@localhost:5432/db", want: "postgres"},
		{name: "其他走 MySQL", dsn: "root:pw@tcp(localhost:3306)/whats_api", want: "mysql"},
		{
			name: "主库拒绝 ClickHouse", dsn: "clickhouse://localhost:9000", isLog: false,
			wantErr: "主库不支持 ClickHouse",
		},
		{
			name: "日志库遇到 ClickHouse 明确报未实现", dsn: "clickhouse://localhost:9000", isLog: true,
			wantErr: "尚未引入",
		},
		{
			name: "日志库遇到 tcp:// 也明确报未实现", dsn: "tcp://localhost:9000", isLog: true,
			wantErr: "尚未引入",
		},
		{
			name: "主库遇到 http:// 也拒绝", dsn: "http://localhost:8123", isLog: false,
			wantErr: "主库不支持 ClickHouse",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dialector, kind, err := chooseDB(c.dsn, c.isLog)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("chooseDB(%q, isLog=%v) 应该报错", c.dsn, c.isLog)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("错误信息应包含 %q，实际 %q", c.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("chooseDB(%q) 报错: %v", c.dsn, err)
			}
			if dialector.Name() != c.want {
				t.Errorf("驱动 = %q，期望 %q", dialector.Name(), c.want)
			}
			if string(kind) != c.want {
				t.Errorf("dbKind = %q，期望 %q", kind, c.want)
			}
		})
	}
}

func TestSQLiteFileResolution(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", common.DefaultSQLiteFile},
		{"local", common.DefaultSQLiteFile},
		{"local:", common.DefaultSQLiteFile},
		{"local:/data/whats-api.db", "/data/whats-api.db"},
		{"local:  /data/x.db  ", "/data/x.db"},
	}
	for _, c := range cases {
		if got := sqliteFile(c.in); got != c.want {
			t.Errorf("sqliteFile(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// MySQL 的 parseTime=false（默认）会让时间列扫不出来；只在缺失时补。
func TestEnsureParseTime(t *testing.T) {
	cases := []struct{ in, want string }{
		{"root:pw@tcp(h:3306)/db", "root:pw@tcp(h:3306)/db?parseTime=true"},
		{"root:pw@tcp(h:3306)/db?charset=utf8mb4", "root:pw@tcp(h:3306)/db?charset=utf8mb4&parseTime=true"},
		{"root:pw@tcp(h:3306)/db?parseTime=true", "root:pw@tcp(h:3306)/db?parseTime=true"},
	}
	for _, c := range cases {
		if got := ensureParseTime(c.in); got != c.want {
			t.Errorf("ensureParseTime(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 方言 helper 的核心逻辑做成"传入方言名"的纯函数，
// 于是三个分支都能在没有真实 MySQL/PG 的机器上测到。
func TestDialectLiterals(t *testing.T) {
	cases := []struct {
		dialect  string
		keyCol   string
		trueVal  string
		falseVal string
	}{
		{"sqlite", "`key`", "1", "0"},
		{"mysql", "`key`", "1", "0"},
		{"postgres", `"key"`, "true", "false"},
	}
	for _, c := range cases {
		if got := keyColFor(c.dialect); got != c.keyCol {
			t.Errorf("keyColFor(%q) = %q，期望 %q", c.dialect, got, c.keyCol)
		}
		if got := trueValFor(c.dialect); got != c.trueVal {
			t.Errorf("trueValFor(%q) = %q，期望 %q", c.dialect, got, c.trueVal)
		}
		if got := falseValFor(c.dialect); got != c.falseVal {
			t.Errorf("falseValFor(%q) = %q，期望 %q", c.dialect, got, c.falseVal)
		}
	}
}

// SQLite 没有行锁语法：lockForUpdate 必须原样返回，不能生成 FOR UPDATE。
func TestLockForUpdateOnSQLiteEmitsNothing(t *testing.T) {
	withTestDB(t)

	var rows []Option
	stmt := lockForUpdateFor("sqlite", DB.Model(&Option{})).
		Session(&gorm.Session{DryRun: true}).
		Find(&rows).Statement

	sql := stmt.SQL.String()
	if strings.Contains(strings.ToUpper(sql), "FOR UPDATE") {
		t.Fatalf("SQLite 下不该生成 FOR UPDATE，实际 SQL: %s", sql)
	}
}

// lazySQLitePool 返回一个永不真正连接的 *sql.DB。
// sql.Open 是惰性的；只要 dialector 不查版本、gorm 不 Ping，它就一次都不会连。
// 它在这里只充当 database/sql 接口的占位实现。
func lazySQLitePool(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开占位连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// withDialector 构造一个不下任何真实服务器的 *gorm.DB：dialector 决定 SQL 怎么渲染，
// ConnPool 只提供接口。于是"MySQL 下真的会生成 FOR UPDATE 吗"这种问题
// 可以不依赖服务器在本机验证。
func withDialector(t *testing.T, d gorm.Dialector) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(d, &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("构造 %T 失败: %v", d, err)
	}
	return db
}

// 非 SQLite 必须真的生成出 FOR UPDATE。
//
// 这里渲染的是**真实方言的 SQL**，不是"子句挂没挂上去"：dialector 决定 SQL 文本，
// 而 MySQL/PostgreSQL 的 dialector 都能在没有服务器时构造出来（ConnPool 只当接口占位）。
// 这一条恰好能挡住最阴险的那个 bug：用 tx.Set("gorm:query_option", "FOR UPDATE") 时
// GORM v2 会**静默忽略**，生成出的 SQL 里根本没有 FOR UPDATE，而代码看起来"加了锁"。
func TestLockForUpdateRendersForUpdateOnServers(t *testing.T) {
	cases := []struct {
		name string
		d    gorm.Dialector
	}{
		{"mysql", mysql.New(mysql.Config{
			Conn:                      lazySQLitePool(t),
			SkipInitializeWithVersion: true,
		})},
		{"postgres", postgres.New(postgres.Config{Conn: lazySQLitePool(t)})},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := withDialector(t, c.d)
			tx := lockForUpdateFor(c.name, db.Session(&gorm.Session{DryRun: true}))
			stmt := tx.Model(&Option{}).Find(&[]Option{}).Statement
			if sqlText := stmt.SQL.String(); !strings.Contains(sqlText, "FOR UPDATE") {
				t.Errorf("%s 下应生成 FOR UPDATE，实际 SQL: %s", c.name, sqlText)
			}
		})
	}
}

func TestDSNKindDoesNotLeakDSN(t *testing.T) {
	restoreEnvGlobals(t)

	cases := []struct {
		dsn  string
		want string
	}{
		{"", "file"},
		{"local:/data/x.db", "file"},
		{"local::memory:", "memory"},
		{"postgres://u:p@h/db", "server"},
		{"root:pw@tcp(h:3306)/db", "server"},
	}
	for _, c := range cases {
		t.Setenv("SQL_DSN", c.dsn)
		common.InitEnv()
		if got := DSNKind(); got != c.want {
			t.Errorf("DSNKind() 对 %q = %q，期望 %q", c.dsn, got, c.want)
		}
	}
	t.Setenv("SQL_DSN", "")
	common.InitEnv()
}

// 启动路径的错误会原样进日志，而 DSN 里通常带密码：任何一处回显都是凭据泄露。
func TestChooseDBErrorDoesNotLeakPassword(t *testing.T) {
	const secret = "sup3rs3cr3t"
	for _, dsn := range []string{
		"clickhouse://user:" + secret + "@localhost:9000",
		"https://user:" + secret + "@localhost:8123",
	} {
		for _, isLog := range []bool{false, true} {
			_, _, err := chooseDB(dsn, isLog)
			if err == nil {
				t.Fatalf("chooseDB(%q, isLog=%v) 应该报错", dsn, isLog)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), dsn) {
				t.Errorf("错误信息泄露出 DSN: %q", err.Error())
			}
		}
	}
}

// 重复 InitDB 不能把上一个连接池留成孤儿：旧池必须被关掉。
// "已关闭"是可观测的——先抓住旧句柄，第二次 Init 之后 Ping 它会失败。
func TestInitDBClosesPreviousHandle(t *testing.T) {
	restoreEnvGlobals(t)

	dir := t.TempDir()
	t.Setenv("LOG_SQL_DSN", "")
	t.Cleanup(func() {
		_ = CloseDB()
		OptionMap = map[string]string{}
	})

	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "first.db"))
	common.InitEnv()
	if err := InitDB(); err != nil {
		t.Fatalf("第一次 InitDB 失败: %v", err)
	}
	first := DB
	firstPool, err := first.DB()
	if err != nil {
		t.Fatalf("取旧连接池失败: %v", err)
	}

	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "second.db"))
	common.InitEnv()
	if err := InitDB(); err != nil {
		t.Fatalf("第二次 InitDB 失败: %v", err)
	}

	if DB == first {
		t.Fatal("第二次 InitDB 应换成新句柄")
	}
	if err := firstPool.Ping(); err == nil {
		t.Errorf("第二次 InitDB 应关掉第一个连接池，但它仍然活着")
	}
	if !PingMain() {
		t.Errorf("第二次 InitDB 之后新主库应可用")
	}
}

// 主库该有的表一张不少，logs 必须落在日志库——这是 08 篇"日志库分离"的接缝，
// M0 就把 logs 放进日志库，等于当下就验证了这条接缝是通的。
func TestLogsLiveInSeparateLogDB(t *testing.T) {
	restoreEnvGlobals(t)

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "main.db"))
	t.Setenv("LOG_SQL_DSN", "local:"+filepath.Join(dir, "log.db"))
	t.Setenv("NODE_TYPE", "master")
	common.InitEnv()
	t.Cleanup(func() { _ = CloseDB(); OptionMap = map[string]string{} })

	if err := InitDB(); err != nil {
		t.Fatalf("InitDB 失败: %v", err)
	}
	if err := InitLogDB(); err != nil {
		t.Fatalf("InitLogDB 失败: %v", err)
	}

	if LOG_DB == DB {
		t.Fatal("配置了独立 LOG_SQL_DSN 时不该复用主库连接")
	}
	if !LogDBSeparate() {
		t.Error("LogDBSeparate() 应为 true")
	}
	if !LOG_DB.Migrator().HasTable("logs") {
		t.Error("logs 表应建在日志库")
	}
	if DB.Migrator().HasTable("logs") {
		t.Error("logs 表不该出现在主库")
	}
	for _, table := range []string{"users", "tokens", "channels", "abilities", "options", "setup"} {
		if !DB.Migrator().HasTable(table) {
			t.Errorf("主库缺少表 %q", table)
		}
	}
	if LOG_DB.Migrator().HasTable("users") {
		t.Error("业务表不该建在日志库")
	}
}

// 迁移幂等：连续三次启动不能有任何副作用（第八条上线验收清单里的那一条）。
func TestMigrateIsIdempotentAcrossRestarts(t *testing.T) {
	withTestDB(t) // 第一次迁移已经在 InitDB/InitLogDB 里跑过了

	for i := 2; i <= 3; i++ {
		if err := Migrate(); err != nil {
			t.Fatalf("第 %d 次主库迁移失败: %v", i, err)
		}
		if err := migrateLOGDB(); err != nil {
			t.Fatalf("第 %d 次日志库迁移失败: %v", i, err)
		}
		if err := CheckSetup(); err != nil {
			t.Fatalf("第 %d 次 CheckSetup 失败: %v", i, err)
		}
	}

	var count int64
	if err := DB.Model(&Setup{}).Count(&count).Error; err != nil {
		t.Fatalf("统计 setup 行数失败: %v", err)
	}
	if count != 1 {
		t.Errorf("setup 表应恰好 1 行，得到 %d 行", count)
	}
}

// slave 节点不建表：多实例同时 AutoMigrate 只会互相抢锁。
func TestSlaveSkipsMigration(t *testing.T) {
	restoreEnvGlobals(t)

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "slave.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "slave")
	common.InitEnv()
	t.Cleanup(func() { _ = CloseDB(); OptionMap = map[string]string{} })

	if err := InitDB(); err != nil {
		t.Fatalf("slave 节点 InitDB 不该失败: %v", err)
	}
	if err := InitLogDB(); err != nil {
		t.Fatalf("slave 节点 InitLogDB 不该失败: %v", err)
	}

	for _, table := range []string{"users", "tokens", "channels", "abilities", "options", "setup", "logs"} {
		if DB.Migrator().HasTable(table) {
			t.Errorf("slave 节点不该建表，但 %q 存在", table)
		}
	}
}

// setup 首次写入恰好一行，重复调用不改动已有行。
func TestCheckSetupWritesExactlyOnce(t *testing.T) {
	withTestDB(t)

	if err := CheckSetup(); err != nil {
		t.Fatalf("CheckSetup 失败: %v", err)
	}
	var first Setup
	if err := DB.First(&first, 1).Error; err != nil {
		t.Fatalf("setup 首行不存在: %v", err)
	}
	if first.Version != common.Version {
		t.Errorf("版本 = %q，期望 %q", first.Version, common.Version)
	}
	if first.InitializedAt <= 0 {
		t.Errorf("初始化时间戳应为正数，得到 %d", first.InitializedAt)
	}

	if err := CheckSetup(); err != nil {
		t.Fatalf("第二次 CheckSetup 失败: %v", err)
	}
	var second Setup
	if err := DB.First(&second, 1).Error; err != nil {
		t.Fatal(err)
	}
	if second.InitializedAt != first.InitializedAt {
		t.Error("已存在时不该改写 setup 行（否则每次重启都显得像首次安装）")
	}

	done, err := SetupComplete()
	if err != nil || !done {
		t.Errorf("SetupComplete() = (%v, %v)，期望 (true, nil)", done, err)
	}
}

// 列名是 ABI 的一部分：写错列名会让将来的数据迁移对不上。
// 这条测试同时钉住"不自行扩列"与"不用 GORM 软删除"。
func TestSchemaMatchesBlueprint(t *testing.T) {
	withTestDB(t)

	mainModels := []struct {
		model   any
		columns []string
	}{
		{&User{}, []string{"id", "username", "password_hash", "role", "status",
			"quota", "used_quota", "group_name", "email", "auth_version", "created_at"}},
		{&Token{}, []string{"id", "user_id", "key", "status", "expired_time", "remain_quota",
			"unlimited_quota", "model_limits_enabled", "model_limits", "allow_ips",
			"used_quota", "group_name", "accessed_at", "created_at"}},
		{&Channel{}, []string{"id", "type", "key", "base_url", "models", "group_name",
			"priority", "weight", "tag", "status", "model_mapping", "param_override",
			"header_override", "setting", "created_at"}},
		{&Ability{}, []string{"group_name", "model_name", "channel_id", "enabled",
			"priority", "weight", "tag"}},
		{&Option{}, []string{"key", "value"}},
		{&Setup{}, []string{"id", "version", "initialized_at"}},
	}

	for _, c := range mainModels {
		for _, col := range c.columns {
			if !DB.Migrator().HasColumn(c.model, col) {
				t.Errorf("%T 缺少列 %q", c.model, col)
			}
		}
		if DB.Migrator().HasColumn(c.model, "deleted_at") {
			t.Errorf("%T 出现了 deleted_at：本项目不用 GORM 软删除（账本只追加）", c.model)
		}
	}

	logColumns := []string{"id", "user_id", "type", "content", "model_name", "quota",
		"prompt_tokens", "completion_tokens", "use_time", "is_stream", "channel_id",
		"group_name", "token_name", "ip", "request_id", "other", "created_at"}
	for _, col := range logColumns {
		if !LOG_DB.Migrator().HasColumn(&Log{}, col) {
			t.Errorf("logs 缺少列 %q", col)
		}
	}

	// abilities 的复合主键是这张表存在的全部意义：一行 = (分组, 模型, 渠道)。
	//
	// 注：GORM v2 的 Migrator 接口没有 PrimaryKeys（计划里写的那条 API 不存在），
	// 所以这里用 schema.Parse 读出模型的复合主键列。
	sch, err := schema.Parse(&Ability{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("解析 abilities schema 失败: %v", err)
	}
	got := map[string]bool{}
	for _, k := range sch.PrimaryFieldDBNames {
		got[k] = true
	}
	for _, want := range []string{"group_name", "model_name", "channel_id"} {
		if !got[want] {
			t.Errorf("abilities 复合主键缺少 %q（实际 %v）", want, sch.PrimaryFieldDBNames)
		}
	}

	// setup 的表名是单数，不跟 GORM 的复数化。
	if !DB.Migrator().HasTable("setup") {
		t.Error("setup 表名应为单数 setup")
	}
}
