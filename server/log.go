package server

import (
	"fmt"
	"os"
	"time"
)

// 极简日志（stderr；stdout 只走凭证交接通道）。
//
// 注意：绝不能把凭证内容打进日志——stderr 会被主进程原样落进 electron-dev.log。
func logInfo(format string, args ...any) {
	emit("INFO", format, args...)
}

func logWarn(format string, args ...any) {
	emit("WARN", format, args...)
}

func logError(format string, args ...any) {
	emit("ERROR", format, args...)
}

func emit(level, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s | %-5s | quaver-server | %s\n",
		time.Now().Format("2006-01-02 15:04:05"), level, fmt.Sprintf(format, args...))
}

// Fatalf 致命错误（启动阶段）。
func Fatalf(format string, args ...any) {
	emit("FATAL", format, args...)
	os.Exit(1)
}

// LogInfo 对外暴露的信息日志（main 用）。
func LogInfo(format string, args ...any) { logInfo(format, args...) }
