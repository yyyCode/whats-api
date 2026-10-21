package middleware

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

// accessedAtThrottleSeconds：令牌最近使用时间的最小刷新间隔。
// 每个请求都写一次会让 SQLite 的单写者把并发请求串行化；
// 60 秒的精度对"这个令牌还活着吗"足够。
const accessedAtThrottleSeconds = 60

// TokenAuth 校验 Authorization: Bearer <key>，把身份写进 gin.Context。
//
// M1 直查数据库（令牌 key 有唯一索引）；缓存是 M3 的事。
// 模型白名单不在这里查：它需要请求体里的 model，由 handler 解析后校验。
func TokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := extractTokenKey(c)
		if key == "" {
			types.NewAuthenticationError("缺少 API Key（请使用 Authorization: Bearer <key>）").Write(c)
			c.Abort()
			return
		}

		token, err := model.GetTokenByKey(key)
		if err != nil {
			if errors.Is(err, model.ErrNotFound) {
				types.NewAuthenticationError("API Key 无效").Write(c)
			} else {
				logger.Error("查询令牌失败", "error", err)
				types.NewInternalError("鉴权服务暂时不可用").Write(c)
			}
			c.Abort()
			return
		}

		now := time.Now().Unix()
		switch {
		case token.Status != constant.TokenStatusEnabled:
			types.NewAuthenticationError("API Key 已被停用").Write(c)
			c.Abort()
			return
		case token.ExpiredTime != -1 && token.ExpiredTime < now:
			types.NewAuthenticationError("API Key 已过期").Write(c)
			c.Abort()
			return
		case !token.UnlimitedQuota && token.RemainQuota <= 0:
			types.NewQuotaError("API Key 额度已用尽").Write(c)
			c.Abort()
			return
		}

		user, err := model.GetUserByID(token.UserID)
		if err != nil {
			if errors.Is(err, model.ErrNotFound) {
				types.NewAuthenticationError("API Key 所属用户不存在").Write(c)
			} else {
				logger.Error("查询用户失败", "error", err)
				types.NewInternalError("鉴权服务暂时不可用").Write(c)
			}
			c.Abort()
			return
		}
		if user.Status != constant.UserStatusEnabled {
			types.NewPermissionError("用户已被停用").Write(c)
			c.Abort()
			return
		}

		group := token.GroupName
		if strings.TrimSpace(group) == "" {
			group = user.GroupName
		}

		relaycommon.SetAuthInfo(c, &relaycommon.AuthInfo{
			TokenID: token.ID,
			UserID:  token.UserID,
			// TokenName 留空：Token 模型还没有 name 列，而把明文 key 放进上下文
			// 只会给未来的日志埋一个泄漏点。
			Group:              group,
			UnlimitedQuota:     token.UnlimitedQuota,
			ModelLimitsEnabled: token.ModelLimitsEnabled,
			ModelLimits:        token.ModelLimits,
		})

		if now-token.AccessedAt >= accessedAtThrottleSeconds {
			if err := model.TouchTokenAccessed(token.ID, now); err != nil {
				// 使用时间刷新失败不影响鉴权结果：它是观测数据，不是正确性条件。
				logger.Warn("刷新令牌使用时间失败", "token_id", token.ID, "error", err)
			}
		}

		c.Next()
	}
}

// extractTokenKey 支持标准 Bearer 头，也接受裸 key 与 X-Api-Key（兼容各类 SDK）。
func extractTokenKey(c *gin.Context) string {
	if h := strings.TrimSpace(c.GetHeader("Authorization")); h != "" {
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			return strings.TrimSpace(h[len("bearer "):])
		}
		return h
	}
	return strings.TrimSpace(c.GetHeader("X-Api-Key"))
}
