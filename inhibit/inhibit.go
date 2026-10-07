// Package inhibit —— 「播放音频时睡眠禁止」的系统侧执行器（设置 [Playing] InhibitSleep）。
//
// 口径三平台一致：只阻止系统睡眠，不碰屏幕 —— 屏幕熄灭/变暗/锁屏策略照常生效。
//   - Linux   xdg-desktop-portal Inhibit，flags 只置 Suspend 位（不置 Idle）
//   - Windows PowerRequestSystemRequired（不碰 PowerRequestDisplayRequired）
//   - macOS   IOKit PreventUserIdleSystemSleep（头文件明示「显示器可变暗熄屏，系统不许空闲睡」）
//
// 持有方是本 sidecar 进程，进程退出时 OS 自动回收：门户随 D-Bus 断开释放、Windows 句柄
// 随进程关闭、macOS 断言随进程死亡移除 —— UI 崩掉最多多禁止到 sidecar 退出，
// 不会出现「应用没了系统却永不睡」。
package inhibit

import (
	"errors"
	"sync"
)

// Logf 由宿主注入的日志钩子（stderr；默认 no-op）。level 取 "INFO"/"WARN"；
// 只有转移与降级才打，幂等重入不刷屏。
var Logf = func(_ string, format string, _ ...any) {}

// Inhibitor 持有至多一个系统级「禁止睡眠」请求；Acquire/Release 幂等（重复调用 no-op）。
// 底层失败不改变 active：下次 Acquire 会重试（总线重连、引擎恢复等自愈路径）。
type Inhibitor struct {
	mu      sync.Mutex
	backend platformBackend
	// supported = 本编译目标有平台实现；false 时 reason 说明原因。
	supported bool
	reason    string
	active    bool
}

// platformBackend 各平台实现：acquire 幂等，release 容忍「从未 acquire」。
type platformBackend interface {
	acquire() error
	release()
}

// New 组装平台执行器。支持与否在编译期定型（平台文件内 newBackend）。
func New() *Inhibitor {
	b, err := newBackend()
	if err != nil {
		return &Inhibitor{supported: false, reason: err.Error()}
	}
	return &Inhibitor{backend: b, supported: true}
}

// Acquire 开始禁止睡眠。返回 (supported, err)：supported=false 表示本平台无实现；
// supported=true 且 err=nil 表示已持有；err != nil 表示本次尝试失败（未持有）。
func (i *Inhibitor) Acquire() (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.supported {
		return false, errors.New("睡眠禁止：此平台未实现（" + i.reason + "）")
	}
	if i.active {
		return true, nil
	}
	if err := i.backend.acquire(); err != nil {
		return true, err
	}
	i.active = true
	Logf("INFO", "睡眠禁止：已持有（播放中）")
	return true, nil
}

// Release 恢复正常睡眠策略。幂等；不支持的平台为 no-op。
func (i *Inhibitor) Release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.supported || !i.active {
		return
	}
	i.backend.release()
	i.active = false
	Logf("INFO", "睡眠禁止：已释放")
}

// Status 供接口层展示：supported=平台有实现；active=当前是否持有；reason=不支持时的原因文案。
func (i *Inhibitor) Status() (supported, active bool, reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.supported, i.active, i.reason
}
