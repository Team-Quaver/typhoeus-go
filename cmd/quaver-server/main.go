// Quaver sidecar（Go 实现）启动入口。
//
// 默认监听 http://127.0.0.1:3200（QUAVER_PORT 可覆盖）。
// 由 Electron 主进程拉起时：QUAVER_CONFIG_DIR 指定配置目录、
// QUAVER_CREDENTIAL_MODE=external 声明凭证经 stdin/stdout 交接。
package main

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/team-quaver/typhoeus-go/server"
)

func main() {
	port := 3200
	if v := os.Getenv("QUAVER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	app, err := server.NewApp()
	if err != nil {
		server.Fatalf("初始化失败: %v", err)
	}
	srv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(port),
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	server.LogInfo("quaver sidecar (Go) 监听 http://127.0.0.1:%d", port)
	if err := srv.ListenAndServe(); err != nil {
		server.Fatalf("HTTP 服务退出: %v", err)
	}
}
