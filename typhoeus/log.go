package typhoeus

// 诊断日志钩子：typhoeus 是纯协商/中继库（无副作用），自己不持有日志设施；
// server 包在组装时注入 logWarn，测试环境保持 nil（no-op）。
// 约定：只打判定原因与数字，不打 URL/vkey/ekey 等敏感值。
var Logf func(format string, args ...any)

func logf(format string, args ...any) {
	if Logf != nil {
		Logf(format, args...)
	}
}
