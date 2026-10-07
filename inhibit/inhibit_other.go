//go:build !linux && !windows && !darwin

// 其余构建目标（js/wasm 等）：没有可持有的系统睡眠接口。newBackend 返回错误，
// New() 会把 supported=false 与原因带给接口层 —— Acquire 报错、Release 无副作用。
package inhibit

import "errors"

func newBackend() (platformBackend, error) {
	return nil, errors.New("此构建目标未提供系统睡眠接口")
}
