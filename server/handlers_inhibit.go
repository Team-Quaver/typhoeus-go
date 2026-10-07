package server

import "net/http"

// ===================== 睡眠禁止（[Playing] InhibitSleep 的执行端） =====================
//
// 渲染层按播放态调用：开始播放且开关开 → POST {active:true}；暂停/停止 → POST
// {active:false}。语义幂等，重复 POST 无副作用 —— 桥接层有 30s 心跳重申期望态，
// sidecar 重启或请求丢失后自愈。响应 {supported, active, reason}：supported=false
// 表示当前编译目标没有平台实现；active=false+reason 表示本次尝试失败（如无 D-Bus）。

func (a *App) handleInhibit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active bool `json:"active"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Active {
		if _, err := a.inhibit.Acquire(); err != nil {
			logWarn("%v", err)
		}
	} else {
		a.inhibit.Release()
	}
	supported, active, reason := a.inhibit.Status()
	writeOK(w, map[string]any{"supported": supported, "active": active, "reason": reason})
}
