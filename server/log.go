package server

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// 极简日志（stderr；stdout 只走凭证交接通道）。
//
// 注意：绝不能把凭证内容打进日志——stderr 会被主进程原样落进 electron-dev.log。
//
// 等级由 QUAVER_LOG_LEVEL 控制（debug|info|warn|error，默认 info）。debug 是给
// 「播放器 seek/切歌造成的良性断流」这类高频噪声留的口子：默认不打，需要排查时
// 再放开，而不是把它们塞进 WARN 里常年刷屏——噪声常态化就等于没有日志。
type severity int

const (
	sevDebug severity = iota
	sevInfo
	sevWarn
	sevError
	sevFatal
)

var minLevel = parseSeverity(os.Getenv("QUAVER_LOG_LEVEL"))

// crotchetEnabled 与 Electron 的 QUAVER_CROTCHET 判定同口径：非空且不是 0/false。
// 音响力成功流水属于高频诊断信息，只在 Crotchet 下写完整日志。
func crotchetEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("QUAVER_CROTCHET")))
	return raw != "" && raw != "0" && raw != "false"
}

func parseSeverity(raw string) severity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return sevDebug
	case "", "info":
		return sevInfo
	case "warn", "warning":
		return sevWarn
	case "error":
		return sevError
	default:
		return sevInfo // 不认识的值按默认放行，别把日志吞了
	}
}

func rankOf(level string) severity {
	switch level {
	case "DEBUG":
		return sevDebug
	case "INFO":
		return sevInfo
	case "WARN":
		return sevWarn
	case "ERROR":
		return sevError
	case "FATAL":
		return sevFatal
	default:
		return sevInfo
	}
}

func logDebug(format string, args ...any) {
	emit("DEBUG", format, args...)
}

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
	if rankOf(level) < minLevel {
		return
	}
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
