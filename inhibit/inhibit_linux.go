//go:build linux

// Linux —— xdg-desktop-portal「Inhibit」优先，未生效则 Logind 直连兜底。
//
// ModeSleep 只阻止系统睡眠；ModeIdle 阻止桌面进入空闲，从而保持画廊屏幕亮起。
//
// ① 门户路径（org.freedesktop.portal.Inhibit，规范 v3）：
//
//	Inhibit(IN window s, IN flags u, IN options a{sv}, OUT handle o)
//	flags：0x1=Logout 0x2=UserSwitch 0x4=Suspend 0x8=Idle —— 按 Mode 置位
//
// 流程：先订 Request::Response 再调用（method_return 与 Response 常同段抵达），
// 收到 code!=0 视为拒绝。持有 = Request 句柄存活；释放 = Request.Close()；
// 会话结束/总线断开时门户自动释放。桌面实现把请求转交会话管理器/电源守护。
//
// Response=0 即视为门户成功。不要要求 logind ListInhibitors 出现对应行：门户
// 可能将请求转给桌面会话电源服务，不会在 logind 暴露同一条 inhibitor。
// D-Bus 连接/方法调用/Response 无响应或明确拒绝时，再通过 system bus 尝试
// org.freedesktop.login1.Manager（systemd-logind 或 elogind）。
//
// ② Logind / elogind 直连（org.freedesktop.login1.Manager）：
//
//	Inhibit("sleep"/"idle", "Quaver", why, "block") → h fd
//
// fd 开着即持有，Close 即释放，进程退出自动回收；polkit 对活跃会话默认放行
// （inhibit-block-sleep allow_active=yes）。会话总线都不可用的环境也能走（system 总线独立）。
//
// 两条路径持有期都以本 sidecar 进程存亡为准，UI 崩掉不会留下「永不睡」的系统。
package inhibit

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalName       = "org.freedesktop.portal.Desktop"
	portalPath       = "/org/freedesktop/portal/desktop"
	portalInhibitIfe = "org.freedesktop.portal.Inhibit"
	portalRequestIfe = "org.freedesktop.portal.Request"

	login1Name = "org.freedesktop.login1"
	login1Path = "/org/freedesktop/login1"
	login1Ife  = "org.freedesktop.login1.Manager"

	inhibitSuspend = 0x4
	inhibitIdle    = 0x8
	responseWait   = 4 * time.Second
	logindWho      = "Quaver"
	logindMode     = "block"
)

type linuxBackend struct {
	mu   sync.Mutex
	mode Mode
	// 两条路径互斥：acquire 时定道，持有期不换道（release 各走各的清理）
	session   *dbus.Conn      // 会话总线（门户路径）
	request   dbus.ObjectPath // 门户 Request 句柄（非空 = 门户路径持有）
	system    *dbus.Conn      // 系统总线（Logind 路径）
	inhibitFD dbus.UnixFD     // logind 持有 fd（>=0 = Logind 路径持有）
}

func newBackend(mode Mode) (platformBackend, error) {
	return &linuxBackend{mode: mode, inhibitFD: -1}, nil
}

func (b *linuxBackend) acquire() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.request != "" || b.inhibitFD >= 0 {
		return nil // 已持有
	}
	if reason := b.tryPortal(); reason != "" {
		Logf("WARN", "%s禁止：%s，改走 Logind 直连", b.mode.label(), reason)
		if err := b.acquireLogin1(); err != nil {
			return fmt.Errorf("%s禁止：%s；Logind 直连也失败: %w", b.mode.label(), reason, err)
		}
	}
	return nil
}

// tryPortal 走一遍门户持有流程。空串 = 成功持有；否则返回未生效原因（门户侧已清理干净）。
func (b *linuxBackend) tryPortal() string {
	conn, err := b.sessionBus()
	if err != nil {
		return err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), responseWait)
	defer cancel()
	ch := make(chan *dbus.Signal, 8)
	match := []dbus.MatchOption{
		dbus.WithMatchInterface(portalRequestIfe),
		dbus.WithMatchMember("Response"),
	}
	if err := conn.AddMatchSignalContext(ctx, match...); err != nil {
		return fmt.Sprintf("订阅门户响应失败（%v）", err)
	}
	conn.Signal(ch)
	defer func() {
		conn.RemoveSignal(ch)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cleanupCancel()
		_ = conn.RemoveMatchSignalContext(cleanupCtx, match...)
	}()

	var request dbus.ObjectPath
	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant("quaver" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)),
		"reason":       dbus.MakeVariant(b.reason()),
	}
	call := conn.Object(portalName, portalPath).CallWithContext(ctx, portalInhibitIfe+".Inhibit", 0, "", b.portalFlags(), opts)
	if err := call.Store(&request); err != nil {
		return fmt.Sprintf("门户 Inhibit 不可用（%v）", err)
	}

	var code uint32
wait:
	for {
		select {
		case sig, ok := <-ch:
			if !ok {
				b.closeRequest(request)
				return "门户响应通道关闭"
			}
			if sig.Path != request {
				continue // 别的应用的门户响应，继续等自己的
			}
			if len(sig.Body) == 0 {
				b.closeRequest(request)
				return "门户 Response 信号缺少结果码"
			}
			var valid bool
			code, valid = sig.Body[0].(uint32)
			if !valid {
				b.closeRequest(request)
				return "门户 Response 信号结果码类型无效"
			}
			break wait
		case <-ctx.Done():
			b.closeRequest(request)
			return fmt.Sprintf("门户 D-Bus 调用或 Response 超时（%v）", responseWait)
		}
	}
	if code != 0 {
		b.closeRequest(request)
		return fmt.Sprintf("门户拒绝请求（code %d）", code)
	}
	// 门户返回成功响应即作为成功依据。桌面会话服务可能持有实际抑制，
	// 因此不以 systemd-logind/elogind 的 inhibitor 列表作为验收条件。
	b.request = request
	return ""
}

func (b *linuxBackend) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.request != "" {
		if b.session != nil {
			b.closeRequest(b.request)
		}
		b.request = ""
	}
	if b.inhibitFD >= 0 {
		_ = syscall.Close(int(b.inhibitFD)) // fd 关闭即释放，进程退出同样回收
		b.inhibitFD = -1
	}
}

// closeRequest 对门户句柄发 Close（NO_REPLY_EXPECTED，尽力而为）。
// 连接已死时门户早已自动释放，错误吞掉即可。
func (b *linuxBackend) closeRequest(request dbus.ObjectPath) {
	_ = b.session.Object(portalName, request).Call(portalRequestIfe+".Close", dbus.FlagNoReplyExpected)
}

// sessionBus 取会话总线（私有连接，断了整体作废重建，不卡死在全局缓存里）。
func (b *linuxBackend) sessionBus() (*dbus.Conn, error) {
	if b.session != nil {
		if b.session.Connected() {
			return b.session, nil
		}
		b.session = nil
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("会话总线不可用（%v）", err)
	}
	b.session = conn
	return conn, nil
}

// systemBus 取系统总线（Logind 路径用；与会话总线互不影响）。
func (b *linuxBackend) systemBus() (*dbus.Conn, error) {
	if b.system != nil {
		if b.system.Connected() {
			return b.system, nil
		}
		b.system = nil
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("系统总线不可用（%v）", err)
	}
	b.system = conn
	return conn, nil
}

// acquireLogin1 Logind 直连持有：fd 开着即生效。
func (b *linuxBackend) acquireLogin1() error {
	conn, err := b.systemBus()
	if err != nil {
		return err
	}
	var fd dbus.UnixFD
	ctx, cancel := context.WithTimeout(context.Background(), responseWait)
	defer cancel()
	if err := conn.Object(login1Name, login1Path).CallWithContext(ctx, login1Ife+".Inhibit", 0,
		b.logindWhat(), logindWho, b.reason(), logindMode).Store(&fd); err != nil {
		return fmt.Errorf("logind Inhibit 失败（%v）", err)
	}
	if fd < 0 {
		return fmt.Errorf("logind Inhibit 返回无效 fd %d", fd)
	}
	b.inhibitFD = fd
	return nil
}

func (b *linuxBackend) portalFlags() uint32 {
	if b.mode == ModeIdle {
		return inhibitIdle
	}
	return inhibitSuspend
}

func (b *linuxBackend) logindWhat() string {
	if b.mode == ModeIdle {
		return "idle"
	}
	return "sleep"
}

func (b *linuxBackend) reason() string {
	if b.mode == ModeIdle {
		return "Quaver 正在画廊播放"
	}
	return "Quaver 正在播放音频"
}
