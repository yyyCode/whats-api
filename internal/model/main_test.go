package model

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yyyCode/whats-api/internal/common"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// withTestDB 用临时目录里的真实 SQLite 文件建立主库与日志库，
// 而不是内存库：迁移幂等、WAL、多进程共享这些行为都只有真实文件才测得到。
//
// ★ 每个测试最多调用一次。它写的是包级 DB/LOG_DB 单例句柄，
// 在循环里反复调用会让上一轮的连接池变成孤儿（Cleanup 的 LIFO 顺序只关得掉最后一个），
// 于是 t.TempDir 的清理在 Windows 上直接报 "being used by another process"。
func withTestDB(t *testing.T) {
	t.Helper()

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
