//go:build darwin

// macOS —— IOKit 电源断言（IOPMLib，10.7+，无需特权，经 powerd 托管）。
//
//	IOReturn IOPMAssertionCreateWithProperties(CFDictionaryRef AssertionProperties,
//	                                           IOPMAssertionID *AssertionID);  // 0 = kIOReturnSuccess
//	IOReturn IOPMAssertionRelease(IOPMAssertionID AssertionID);
//
// 断言字典（键为 CFString，按内容等值比较 → 必须配 kCFCopyStringDictionaryKeyCallBacks；
// 值回调用 kCFTypeDictionaryValueCallBacks 让字典接管 retain/release）：
//
//	"AssertType"  → CFString "PreventUserIdleSystemSleep"（kIOPMAssertPreventUserIdleSystemSleep）
//	"AssertLevel" → CFNumber(kCFNumberIntType) 255（kIOPMAssertionLevelOn）
//	"AssertName"  → CFString "Quaver"（kIOPMAssertionNameKey：pmset -g assertions 里可见，便于排查）
//
// PreventUserIdleSystemSleep 的官方语义与另两平台同口径：显示器照常变暗/熄屏，
// 系统不许因空闲而睡；合盖、菜单休眠、低电量等其他睡因不受影响。
// 释放 = IOPMAssertionRelease(断言 ID)；kIOPMNullAssertionID=0 不可释放。
//
// 实现经 purego 走 dlopen 调 C ABI —— sidecar 全线零 cgo（CI 以 CGO_ENABLED=0
// 交叉编译为准），purego 在 darwin 上用 go:cgo_import_dynamic 链 dlopen，零 cgo 可用。
// 框架路径写死 /System/Library 前缀：macOS 11+ 系统卷只读且固定，dyld 亦接受。
package inhibit

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	cfFramework    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	iokitFramework = "/System/Library/Frameworks/IOKit.framework/IOKit"

	kCFStringEncodingUTF8 = 0x08000100
	kCFNumberIntType      = 9 // CFNumberType：C int（32 位）
	kIOPMAssertionLevelOn = 255

	assertTypeKey   = "AssertType"                 // kIOPMAssertionTypeKey
	assertLevelKey  = "AssertLevel"                // kIOPMAssertionLevelKey
	assertNameKey   = "AssertName"                 // kIOPMAssertionNameKey
	assertTypeValue = "PreventUserIdleSystemSleep" // kIOPMAssertPreventUserIdleSystemSleep
	assertNameValue = "Quaver"
)

var (
	cf, iokit uintptr
	// kCFCopyStringDictionaryKeyCallBacks / kCFTypeDictionaryValueCallBacks：
	// CoreFoundation 的导出数据符号（结构体常量），Dlsym 取地址后按指针传入。
	keyCallbacks, valueCallbacks uintptr

	cfDictCreateMutable func(alloc uintptr, capacity int, keyCallbacks, valueCallbacks unsafe.Pointer) unsafe.Pointer
	cfStringCreate      func(alloc uintptr, cStr *byte, encoding uint32) unsafe.Pointer
	cfNumberCreate      func(alloc uintptr, typ int, value unsafe.Pointer) unsafe.Pointer
	cfDictSetValue      func(dict, key, value unsafe.Pointer)
	cfRelease           func(ref unsafe.Pointer)

	iopmCreateWithProperties func(dict unsafe.Pointer, out *uint32) int32 // IOReturn
	iopmRelease              func(id uint32) int32                        // IOReturn

	loadOnce sync.Once
	loadErr  error
)

// loadSymbols 惰性装载两个框架并绑定符号；只跑一次，失败结果被记住（每次 Acquire 重试无害）。
func loadSymbols() error {
	loadOnce.Do(func() {
		var err error
		if cf, err = purego.Dlopen(cfFramework, purego.RTLD_LAZY|purego.RTLD_GLOBAL); err != nil {
			loadErr = fmt.Errorf("睡眠禁止：CoreFoundation 加载失败: %w", err)
			return
		}
		if iokit, err = purego.Dlopen(iokitFramework, purego.RTLD_LAZY|purego.RTLD_GLOBAL); err != nil {
			loadErr = fmt.Errorf("睡眠禁止：IOKit 加载失败: %w", err)
			return
		}
		if keyCallbacks, err = purego.Dlsym(cf, "kCFCopyStringDictionaryKeyCallBacks"); err != nil {
			loadErr = fmt.Errorf("睡眠禁止：缺 CF 字典键回调: %w", err)
			return
		}
		if valueCallbacks, err = purego.Dlsym(cf, "kCFTypeDictionaryValueCallBacks"); err != nil {
			loadErr = fmt.Errorf("睡眠禁止：缺 CF 字典值回调: %w", err)
			return
		}
		for _, s := range []struct {
			handle uintptr // 符号所在框架（CF 工具函数在 CoreFoundation，IOPM 在 IOKit）
			name   string
			fn     any
		}{
			{cf, "CFDictionaryCreateMutable", &cfDictCreateMutable},
			{cf, "CFStringCreateWithCString", &cfStringCreate},
			{cf, "CFNumberCreate", &cfNumberCreate},
			{cf, "CFDictionarySetValue", &cfDictSetValue},
			{cf, "CFRelease", &cfRelease},
			{iokit, "IOPMAssertionCreateWithProperties", &iopmCreateWithProperties},
			{iokit, "IOPMAssertionRelease", &iopmRelease},
		} {
			sym, err := purego.Dlsym(s.handle, s.name)
			if err != nil {
				loadErr = fmt.Errorf("睡眠禁止：绑定 %s 失败: %w", s.name, err)
				return
			}
			purego.RegisterFunc(s.fn, sym)
		}
	})
	return loadErr
}

// cfStringOf 造 CFString（alloc=NULL = kCFAllocatorDefault）。C 串缓冲活到调用返回为止
// （CFStringCreateWithCString 会拷贝内容），runtime.KeepAlive 兜 GC。
func cfStringOf(s string) unsafe.Pointer {
	raw := append([]byte(s), 0)
	ref := cfStringCreate(0, &raw[0], kCFStringEncodingUTF8)
	runtime.KeepAlive(&raw)
	return ref
}

type darwinBackend struct {
	mu        sync.Mutex
	assertion uint32 // IOPMAssertionID；0 = kIOPMNullAssertionID = 未持有
}

func newBackend() (platformBackend, error) { return &darwinBackend{}, nil }

func (b *darwinBackend) acquire() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.assertion != 0 {
		return nil // 已持有
	}
	if err := loadSymbols(); err != nil {
		return err
	}

	dict := cfDictCreateMutable(0, 0, unsafe.Pointer(keyCallbacks), unsafe.Pointer(valueCallbacks))
	if dict == nil {
		return fmt.Errorf("睡眠禁止：CFDictionaryCreateMutable 失败")
	}
	defer cfRelease(dict)

	level := uint32(kIOPMAssertionLevelOn)
	typeValue := cfStringOf(assertTypeValue)
	levelValue := cfNumberCreate(0, kCFNumberIntType, unsafe.Pointer(&level))
	nameValue := cfStringOf(assertNameValue)
	typeKey, levelKey, nameKey := cfStringOf(assertTypeKey), cfStringOf(assertLevelKey), cfStringOf(assertNameKey)
	// 字典按 kCFTypeDictionaryValueCallBacks 持有插入值：SetValue 后即可释放本侧引用
	cfDictSetValue(dict, typeKey, typeValue)
	cfDictSetValue(dict, levelKey, levelValue)
	cfDictSetValue(dict, nameKey, nameValue)
	cfRelease(typeValue)
	cfRelease(levelValue)
	cfRelease(nameValue)
	cfRelease(typeKey)
	cfRelease(levelKey)
	cfRelease(nameKey)
	runtime.KeepAlive(level)

	var id uint32
	if ret := iopmCreateWithProperties(dict, &id); ret != 0 {
		return fmt.Errorf("睡眠禁止：IOPMAssertionCreateWithProperties 失败（IOReturn 0x%x）", ret)
	}
	if id == 0 {
		return fmt.Errorf("睡眠禁止：IOPMAssertionCreateWithProperties 返回空断言（kIOPMNullAssertionID）")
	}
	b.assertion = id
	return nil
}

func (b *darwinBackend) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.assertion == 0 {
		return
	}
	if err := loadSymbols(); err == nil {
		if ret := iopmRelease(b.assertion); ret != 0 {
			Logf("WARN", "睡眠禁止：IOPMAssertionRelease 返回 IOReturn 0x%x（随进程仍会回收）", ret)
		}
	}
	b.assertion = 0
}
