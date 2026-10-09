// Package inhibit —— 系统电源策略抑制器。
//
// 支持两种相互独立的模式：
//   - ModeSleep：播放音频时禁止系统睡眠，但允许屏幕按电源策略熄灭
//   - ModeIdle：画廊模式时禁止系统进入空闲，保持屏幕不熄灭
//
// 抑制句柄由 sidecar 持有：门户随 D-Bus 断开释放、Windows 句柄随进程关闭、
// macOS 断言随进程死亡移除。UI 崩掉最多多抑制到 sidecar 退出，不会永久挂住系统策略。
package inhibit

import (
	"errors"
	"sync"
)

// Logf 由宿主注入的日志钩子（stderr；默认 no-op）。level 取 "INFO"/"WARN"；
// 只有转移与降级才打，幂等重入不刷屏。
var Logf = func(_ string, format string, _ ...any) {}

// Inhibitor 持有至多一个系统级电源抑制请求；Acquire/Release 幂等（重复调用 no-op）。
// 底层失败不改变 active：下次 Acquire 会重试（总线重连、引擎恢复等自愈路径）。
type Inhibitor struct {
	mu      sync.Mutex
	backend platformBackend
	mode    Mode
	// supported = 本编译目标有平台实现；false 时 reason 说明原因。
	supported bool
	reason    string
	active    bool
}

// Mode 是系统电源抑制的目标。每个 Inhibitor 只持有一种模式，调用方可各建一个
// 实例，从而让播放中的 sleep 抑制与画廊中的 idle 抑制独立释放。
type Mode uint8

const (
	ModeSleep Mode = iota
	ModeIdle
)

func (m Mode) label() string {
	if m == ModeIdle {
		return "空闲"
	}
	return "睡眠"
}

// platformBackend 各平台实现：acquire 幂等，release 容忍「从未 acquire」。
type platformBackend interface {
	acquire() error
	release()
}

// New 组装旧语义的睡眠抑制器。
func New() *Inhibitor { return NewWithMode(ModeSleep) }

// NewWithMode 组装指定模式的系统电源抑制器。
// 支持与否在编译期定型（平台文件内 newBackend）。
func NewWithMode(mode Mode) *Inhibitor {
	if mode != ModeSleep && mode != ModeIdle {
		mode = ModeSleep
	}
	b, err := newBackend(mode)
	if err != nil {
		return &Inhibitor{mode: mode, supported: false, reason: err.Error()}
	}
	return &Inhibitor{mode: mode, backend: b, supported: true}
}

// Acquire 开始抑制对应电源策略。返回 (supported, err)：supported=false 表示本平台无实现；
// supported=true 且 err=nil 表示已持有；err != nil 表示本次尝试失败（未持有）。
func (i *Inhibitor) Acquire() (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.supported {
		return false, errors.New(i.mode.label() + "禁止：此平台未实现（" + i.reason + "）")
	}
	if i.active {
		return true, nil
	}
	if err := i.backend.acquire(); err != nil {
		return true, err
	}
	i.active = true
	Logf("INFO", "%s禁止：已持有", i.mode.label())
	return true, nil
}

// Release 恢复正常电源策略。幂等；不支持的平台为 no-op。
func (i *Inhibitor) Release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.supported || !i.active {
		return
	}
	i.backend.release()
	i.active = false
	Logf("INFO", "%s禁止：已释放", i.mode.label())
}

// Status 供接口层展示：supported=平台有实现；active=当前是否持有；reason=不支持时的原因文案。
func (i *Inhibitor) Status() (supported, active bool, reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.supported, i.active, i.reason
}
