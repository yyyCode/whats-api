// whats-api 入口。
//
// 这里只做三件事：装信号处理 → 调 app.Run(ctx) → 按错误码退出。
// 所有初始化顺序与停机逻辑都在 app.Run 里，因为它需要被测试直接驱动
// （本机无法向原生进程投递 SIGTERM，信号只是外层的一层薄壳）。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yyyCode/whats-api/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}
