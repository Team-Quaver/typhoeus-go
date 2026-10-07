//go:build linux

// Linux —— 睡眠禁止：xdg-desktop-portal「Inhibit」优先，未生效则 Logind 直连兜底。
//
// 口径（与另两平台一致）：只阻止系统睡眠，屏幕熄灭/变暗/锁屏不受影响。
//
// ① 门户路径（org.freedesktop.portal.Inhibit，规范 v3）：
//
//	Inhibit(IN window s, IN flags u, IN options a{sv}, OUT handle o)
//	flags：0x1=Logout 0x2=UserSwitch 0x4=Suspend 0x8=Idle —— 只置 Suspend
//
// 流程：先订 Request::Response 再调用（method_return 与 Response 常同段抵达），
// 收到 code!=0 视为拒绝。持有 = Request 句柄存活；释放 = Request.Close()；
// 会话结束/总线断开时门户自动释放。桌面实现把请求转交会话管理器/电源守护
// （GNOME=gnome-session、KDE=powerdevil），由它们在 logind 落一个 sleep inhibitor。
//
// ② 假成功检测与 Logind 兜底（真机踩过）：部分桌面（Hyprland/Noctalia 路由到 kde
// portal 而 powerdevil 缺位）门户应答 code=0 却什么都不登记。因此 Response=0 后比对
// logind ListInhibitors 前后快照：本 UID 名下没有新增 sleep 行 → 判「门户未生效」，
// Close 掉请求句柄，改走 Logind 直连。快照比对只认本 UID 的新增 sleep 行，
// 避免把并发时刻其他应用的登记误记到门户头上。
//
// ③ Logind 直连（org.freedesktop.login1.Manager）：
//
//	Inhibit("sleep", "Quaver", why, "block") → h fd
//
// fd 开着即持有，Close 即释放，进程退出自动回收；polkit 对活跃会话默认放行
// （inhibit-block-sleep allow_active=yes）。会话总线都不可用的环境也能走（system 总线独立）。
//
// 两条路径持有期都以本 sidecar 进程存亡为准，UI 崩掉不会留下「永不睡」的系统。
package inhibit

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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

	inhibitSuspend = 0x4 // 只禁止睡眠；不置 0x8(Idle)，屏幕策略不接管
	responseWait   = 4 * time.Second
	verifyGrace    = 400 * time.Millisecond // 会话管理器登记 logind 的宽限

	logindWho  = "Quaver"
	logindWhy  = "Quaver 正在播放音频"
	logindMode = "block"
)

type linuxBackend struct {
	mu sync.Mutex
	// 两条路径互斥：acquire 时定道，持有期不换道（release 各走各的清理）
	session *dbus.Conn      // 会话总线（门户路径）
	request dbus.ObjectPath // 门户 Request 句柄（非空 = 门户路径持有）
	system  *dbus.Conn      // 系统总线（Logind 路径）
	sleepFD dbus.UnixFD     // logind 持有 fd（>=0 = Logind 路径持有）
}

func newBackend() (platformBackend, error) { return &linuxBackend{sleepFD: -1}, nil } // 总线惰性连接

func (b *linuxBackend) acquire() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.request != "" || b.sleepFD >= 0 {
		return nil // 已持有
	}
	if reason := b.tryPortal(); reason != "" {
		Logf("WARN", "睡眠禁止：%s，改走 Logind 直连", reason)
		if err := b.acquireLogin1(); err != nil {
			return fmt.Errorf("睡眠禁止：%s；Logind 直连也失败: %w", reason, err)
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
	// 假成功检测的基线：门户若真生效，桌面守护会在 logind 里替我们登记一行
	before := b.login1Snapshot()

	ch := make(chan *dbus.Signal, 8)
	match := []dbus.MatchOption{
		dbus.WithMatchInterface(portalRequestIfe),
		dbus.WithMatchMember("Response"),
	}
	if err := conn.AddMatchSignal(match...); err != nil {
		return fmt.Sprintf("订阅门户响应失败（%v）", err)
	}
	conn.Signal(ch)
	defer func() {
		conn.RemoveSignal(ch)
		_ = conn.RemoveMatchSignal(match...)
	}()

	var request dbus.ObjectPath
	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant("quaver" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)),
		"reason":       dbus.MakeVariant(logindWhy),
	}
	if err := conn.Object(portalName, portalPath).Call(portalInhibitIfe+".Inhibit", 0, "", uint32(inhibitSuspend), opts).Store(&request); err != nil {
		return fmt.Sprintf("门户 Inhibit 不可用（%v）", err)
	}

	var code uint32
	deadline := time.After(responseWait)
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
			code, _ = sig.Body[0].(uint32)
			break wait
		case <-deadline:
			b.closeRequest(request)
			return fmt.Sprintf("门户 Response 超时（%v）", responseWait)
		}
	}
	if code != 0 {
		b.closeRequest(request)
		return fmt.Sprintf("门户拒绝请求（code %d）", code)
	}
	// code=0 也要验货：桌面守护此刻应在 logind 登记了 sleep 行，否则就是假成功
	if !b.portalEnforced(before) {
		b.closeRequest(request)
		return "门户应答成功但 logind 未登记睡眠抑制（后端假成功）"
	}
	b.request = request
	return ""
}

// portalEnforced 比对前后快照：本 UID 是否新增了含 sleep 的抑制行。
// 给桌面守护一次宽限重查（登记与应答之间可能有微小延迟）。
// logind 不可用（非 systemd）时无从验证，信任门户的 code=0。
func (b *linuxBackend) portalEnforced(before []login1Inhibitor) bool {
	uid := uint32(os.Getuid())
	hasNew := func(rows []login1Inhibitor) bool {
		if rows == nil {
			return true
		}
		for _, row := range rows {
			if row.UID == uid && strings.Contains(row.What, "sleep") && !row.in(before) {
				return true
			}
		}
		return false
	}
	after := b.login1Snapshot()
	if after == nil || hasNew(after) {
		return true
	}
	time.Sleep(verifyGrace)
	return hasNew(b.login1Snapshot())
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
	if b.sleepFD >= 0 {
		_ = syscall.Close(int(b.sleepFD)) // fd 关闭即释放，进程退出同样回收
		b.sleepFD = -1
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
	if err := conn.Object(login1Name, login1Path).Call(login1Ife+".Inhibit", 0,
		"sleep", logindWho, logindWhy, logindMode).Store(&fd); err != nil {
		return fmt.Errorf("logind Inhibit 失败（%v）", err)
	}
	if fd < 0 {
		return fmt.Errorf("logind Inhibit 返回无效 fd %d", fd)
	}
	b.sleepFD = fd
	return nil
}

// login1Inhibitor 对应 ListInhibitors 的行 a(sstsss)：what/who/why/mode/uid/pid。
type login1Inhibitor struct {
	What string
	Who  string
	Why  string
	Mode string
	UID  uint32
	PID  uint32
}

func (row login1Inhibitor) in(rows []login1Inhibitor) bool {
	for _, r := range rows {
		if r == row {
			return true
		}
	}
	return false
}

// login1Snapshot 列出当前 logind 抑制行。logind 不可用返回 nil（调用方据此跳过验货）。
func (b *linuxBackend) login1Snapshot() []login1Inhibitor {
	conn, err := b.systemBus()
	if err != nil {
		return nil
	}
	var rows []login1Inhibitor
	if err := conn.Object(login1Name, login1Path).Call(login1Ife+".ListInhibitors", 0).Store(&rows); err != nil {
		return nil
	}
	return rows
}
