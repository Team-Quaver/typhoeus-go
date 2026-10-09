//go:build windows

// Windows —— 电源请求对象（winbase.h：PowerCreateRequest / PowerSetRequest /
// PowerClearRequest，Kernel32.dll，Win7+，无需特权）。
//
// ModeSleep 置 PowerRequestSystemRequired（枚举值 1），ModeIdle 置
// PowerRequestDisplayRequired（枚举值 0），分别对应阻止系统睡眠与阻止显示器熄灭。
//
// 生命周期：Create（失败返回 INVALID_HANDLE_VALUE）→ Set（激活，进程内可反复
// Set/Clear）→ Clear + CloseHandle 释放；进程退出 OS 自动回收句柄。
// 原因串走 REASON_CONTEXT 的 SimpleReasonString 分支（Version=0、Flags=
// POWER_REQUEST_CONTEXT_SIMPLE_STRING=0x1），电源计划高级设置里可读到。
//
// x/sys/windows 未包装这三个函数，按惯例经 NewLazySystemDLL 绑定 —— 错误路径
// 全部返回 error，绝不 panic（LazyProc 找不到符号时会 panic，先 Find 探测）。
package inhibit

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// POWER_REQUEST_TYPE（winnt.h 枚举序）
	powerRequestDisplayRequired = 0
	powerRequestSystemRequired  = 1
	// REASON_CONTEXT 标志
	reasonContextVersion      = 0 // POWER_REQUEST_CONTEXT_VERSION
	reasonContextSimpleString = 1 // POWER_REQUEST_CONTEXT_SIMPLE_STRING

)

var (
	modkernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procPowerCreateRequest = modkernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = modkernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = modkernel32.NewProc("PowerClearRequest")
)

// reasonContext 是 REASON_CONTEXT 的只消费 SimpleReasonString 分支的投影：
//
//	ULONG Version; DWORD Flags; union { LPWSTR SimpleReasonString; struct {…} Detailed; } Reason;
//
// Win64/ARM64 下前两个 u32 之后 union 指针落在 offset 8（自然对齐），与 C 布局一致。
type reasonContext struct {
	Version uint32
	Flags   uint32
	Reason  *uint16
}

type windowsBackend struct {
	mode   Mode
	handle windows.Handle // 0 = 未持有
}

func newBackend(mode Mode) (platformBackend, error) { return &windowsBackend{mode: mode}, nil }

func (b *windowsBackend) acquire() error {
	if b.handle != 0 {
		return nil // 已持有
	}
	// LazyProc 找不到符号会 panic（不该发生在 Win7+，但别赌）——先 Find 走 error 路径
	if err := procPowerCreateRequest.Find(); err != nil {
		return fmt.Errorf("电源抑制：kernel32 缺少电源请求接口: %w", err)
	}
	why, err := windows.UTF16PtrFromString(b.reason())
	if err != nil {
		return fmt.Errorf("电源抑制：原因串编码失败: %w", err)
	}
	ctx := reasonContext{Version: reasonContextVersion, Flags: reasonContextSimpleString, Reason: why}
	r1, _, lastErr := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	if r1 == 0 || r1 == uintptr(^uintptr(0)) { // NULL（理论外）/ INVALID_HANDLE_VALUE
		return fmt.Errorf("电源抑制：PowerCreateRequest 失败: %v", lastErr)
	}
	h := windows.Handle(r1)
	if r1, _, lastErr := procPowerSetRequest.Call(uintptr(h), uintptr(b.requestType())); r1 == 0 {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("电源抑制：PowerSetRequest 失败: %v", lastErr)
	}
	b.handle = h
	return nil
}

func (b *windowsBackend) release() {
	if b.handle == 0 {
		return
	}
	// Clear 失败不阻断：CloseHandle 后整个请求对象连同激活态一起消失
	_, _, _ = procPowerClearRequest.Call(uintptr(b.handle), uintptr(b.requestType()))
	_ = windows.CloseHandle(b.handle)
	b.handle = 0
}

func (b *windowsBackend) requestType() uint32 {
	if b.mode == ModeIdle {
		return powerRequestDisplayRequired
	}
	return powerRequestSystemRequired
}

func (b *windowsBackend) reason() string {
	if b.mode == ModeIdle {
		return "Quaver 正在画廊播放"
	}
	return "Quaver 正在播放音频"
}
