package modules

import (
	"fmt"
	"os"
	"time"
)

// logWarn 模块层告警日志（与 server 包的日志格式一致，输出到 stderr）。
// modules 不反向依赖 server —— 登录/MQTT 这些长生命周期流程的失败必须可见，
// 不能静默吞掉（手机扫码「确认后没反应」曾经就是这么丢的）。
func logWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s | WARN  | qqmusic-modules | %s\n",
		time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
