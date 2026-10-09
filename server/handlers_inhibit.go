package server

import "net/http"

// ===================== 系统电源抑制执行端 =====================
//
// 渲染层按播放态/画廊态调用：播放音频用 mode=sleep，画廊播放用 mode=idle；
// 缺省 mode 保持旧客户端的 sleep 语义。语义幂等，重复 POST 无副作用 —— 桥接层有
// 30s 心跳重申期望态，sidecar 重启或请求丢失后自愈。响应
// {supported, active, reason}：supported=false 表示当前编译目标没有平台实现；
// active=false+reason 表示本次尝试失败（如无 D-Bus）。

func (a *App) handleInhibit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active bool   `json:"active"`
		Mode   string `json:"mode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	target := a.inhibitSleep
	if body.Mode == "idle" {
		target = a.inhibitIdle
	} else if body.Mode != "" && body.Mode != "sleep" {
		writeErr(w, http.StatusBadRequest, -1, "未知的 inhibit mode: "+body.Mode)
		return
	}
	if body.Active {
		if _, err := target.Acquire(); err != nil {
			logWarn("%v", err)
		}
	} else {
		target.Release()
	}
	supported, active, reason := target.Status()
	writeOK(w, map[string]any{"supported": supported, "active": active, "reason": reason})
}
