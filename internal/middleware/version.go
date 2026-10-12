package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
)

// VersionHeader 是版本响应头。运维不用登服务器就能知道线上跑的是哪个版本。
const VersionHeader = "X-Whats-Api-Version"

func Version() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header(VersionHeader, common.Version)
		c.Next()
	}
}
