package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
)

// withAuthDB 建一个临时库；每个用例自己插数据。
func withAuthDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	common.SQLDSN = "local:" + filepath.Join(dir, "auth.db")
	common.LogSQLDSN = ""
	common.IsMasterNode = true
	logger.SetupLogger("error", false)
	if err := model.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = model.CloseDB() })
}

// seedToken 建一个启用用户 + 令牌，返回令牌。
// key 必须由调用方给出：同一个测试里会建多个令牌，用 t.Name() 会撞唯一索引。
func seedToken(t *testing.T, key string, mutate func(*model.Token)) *model.Token {
	t.Helper()
	u := model.User{Username: "u-" + key, Status: constant.UserStatusEnabled, GroupName: "default"}
	if err := model.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	tok := model.Token{
		Key:            key,
		UserID:         u.ID,
		Status:         constant.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
		GroupName:      "default",
	}
	if mutate != nil {
		mutate(&tok)
	}
	if err := model.DB.Create(&tok).Error; err != nil {
		t.Fatal(err)
	}
	return &tok
}

func authEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/ping", TokenAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return engine
}

func doAuth(engine *gin.Engine, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestTokenAuthRejects(t *testing.T) {
	withAuthDB(t)
	seedToken(t, "sk-enabled", nil)

	disabled := seedToken(t, "sk-disabled", func(tk *model.Token) { tk.Status = constant.TokenStatusDisabled })
	expired := seedToken(t, "sk-expired", func(tk *model.Token) { tk.ExpiredTime = time.Now().Add(-time.Hour).Unix() })
	exhausted := seedToken(t, "sk-exhausted", func(tk *model.Token) { tk.UnlimitedQuota = false; tk.RemainQuota = 0 })

	engine := authEngine()
	cases := []struct {
		name string
		key  string
		want int
	}{
		{"没有 Authorization", "", http.StatusUnauthorized},
		{"未知 key", "sk-nope", http.StatusUnauthorized},
		{"停用的令牌", disabled.Key, http.StatusUnauthorized},
		{"过期的令牌", expired.Key, http.StatusUnauthorized},
		{"额度用尽", exhausted.Key, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := doAuth(engine, c.key).Code; got != c.want {
				t.Errorf("状态码 = %d，期望 %d", got, c.want)
			}
		})
	}
}

func TestTokenAuthRejectsDisabledUser(t *testing.T) {
	withAuthDB(t)
	tok := seedToken(t, "sk-user-disabled", nil)

	var u model.User
	if err := model.DB.First(&u, tok.UserID).Error; err != nil {
		t.Fatal(err)
	}
	u.Status = constant.UserStatusDisabled
	if err := model.DB.Save(&u).Error; err != nil {
		t.Fatal(err)
	}

	if got := doAuth(authEngine(), tok.Key).Code; got != http.StatusForbidden {
		t.Errorf("用户被停用时状态码 = %d，期望 403", got)
	}
}

func TestTokenAuthAcceptsAndPopulatesContext(t *testing.T) {
	withAuthDB(t)
	tok := seedToken(t, "sk-ok", nil)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/ping", TokenAuth(), func(c *gin.Context) {
		a := relaycommon.GetAuthInfo(c)
		if a == nil || a.TokenID != tok.ID || a.UserID != tok.UserID || a.Group != "default" {
			c.String(http.StatusInternalServerError, "auth info 不对: %+v", a)
			return
		}
		c.String(http.StatusOK, "ok")
	})

	w := doAuth(engine, tok.Key)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", w.Code, w.Body.String())
	}

	var got model.Token
	if err := model.DB.First(&got, tok.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.AccessedAt == 0 {
		t.Error("首次鉴权应刷新 accessed_at")
	}
}
