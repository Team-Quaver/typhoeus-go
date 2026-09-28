// Package server 是 Quaver 本地 API sidecar 的 HTTP 层（对标 vendor/Typhoeus/quaver_server）。
//
// 会话/凭证模型（与 Electron 主进程的约定必须保持一致，改一边必须改两边）：
//   - 配置根目录：Linux $XDG_CONFIG_HOME/quaver-music（默认 ~/.config/quaver-music）、
//     Windows %AppData%/Quaver Music、macOS ~/Library/Application Support/Quaver Music。
//     Electron 拉起本进程时会显式下传 QUAVER_CONFIG_DIR。
//   - **凭证由 Electron 主进程独占，本包从不读写凭证明文。** 磁盘上只有密文
//     credential.enc，钥匙在系统密钥管理器里。主进程启动时通过 stdin 注入一行
//     `QCRED1 {json}`（或 `QCRED1 null`）；本进程登录/刷新/登出时往 stdout 回写
//     同一格式，由主进程加密落盘。该模式由 QUAVER_CREDENTIAL_MODE=external 声明。
//   - 不设该变量（手工单跑）＝ memory 模式：登录只驻内存，不落盘也不读盘。
//     宁可这次不持久化，也绝不把明文凭证写进文件。
//   - device.json 是设备指纹（不是密钥），与配置同目录保持明文。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

// HandoffPrefix 凭证交接信封前缀：与 ui/electron/keyring.mjs:HANDOFF_PREFIX 逐字一致
// （含末尾空格）。stdout 上只有带此前缀的行会被主进程当凭证，其余按日志处理。
const HandoffPrefix = "QCRED1 "

// CredentialMode 凭证持久化模式。
type CredentialMode string

// external=交接给主进程加密落盘；memory=只驻内存。没有 file——明文凭证不在选项里。
const (
	ModeExternal CredentialMode = "external"
	ModeMemory   CredentialMode = "memory"
)

// CurrentCredentialMode 从环境读取凭证模式。
func CurrentCredentialMode() CredentialMode {
	if strings.TrimSpace(os.Getenv("QUAVER_CREDENTIAL_MODE")) == "external" {
		return ModeExternal
	}
	return ModeMemory
}

// ConfigDir 配置根目录（与 ui/electron/config.mjs:configDir 保持一致）。
func ConfigDir() string {
	if override := strings.TrimSpace(os.Getenv("QUAVER_CONFIG_DIR")); override != "" {
		return override
	}
	switch runtime.GOOS {
	case "windows":
		base := strings.TrimSpace(os.Getenv("APPDATA"))
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(base, "Quaver Music")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Quaver Music")
	default:
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			return filepath.Join(xdg, "quaver-music")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "quaver-music")
	}
}

// Session 进程级会话：凭证变更集中处理（对标 quaver_server/session.py）。
type Session struct {
	mu   sync.RWMutex
	mode CredentialMode
	cred *qqmusic.Credential
	cl   *qqmusic.Client
	out  *bufio.Writer

	listeners   []func()
	listenersMu sync.Mutex
}

// NewSession 构造会话：初始化客户端（设备指纹持久化到配置目录），
// external 模式下从 stdin 读主进程注入的首行凭证。
func NewSession() (*Session, error) {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建配置目录失败: %w", err)
	}
	mode := CurrentCredentialMode()
	s := &Session{
		mode: mode,
		cred: &qqmusic.Credential{},
		out:  bufio.NewWriter(os.Stdout),
	}
	cl, err := qqmusic.NewClient(s.cred, filepath.Join(dir, "device.json"), "")
	if err != nil {
		return nil, err
	}
	s.cl = cl
	if mode == ModeExternal {
		s.readInjectedCredential()
	} else {
		logInfo("凭证模式=memory（未设 QUAVER_CREDENTIAL_MODE）：登录只在内存里，关掉本进程即需重新登录；" +
			"要持久化请由 Electron 主进程拉起本服务（走 stdin/stdout 交接）")
	}
	return s, nil
}

// readInjectedCredential 读主进程注入的第一行凭证（仅 external 模式；阻塞读）。
func (s *Session) readInjectedCredential() {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		logInfo("父进程未注入凭证（stdin 立即 EOF），按未登录处理")
		return
	}
	body := strings.TrimSpace(line)
	if !strings.HasPrefix(body, HandoffPrefix) {
		logWarn("注入行前缀不匹配，按未登录处理")
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(body, HandoffPrefix))
	if payload == "null" || payload == "" {
		return
	}
	var cred qqmusic.Credential
	if err := json.Unmarshal([]byte(payload), &cred); err != nil {
		logWarn("注入的凭证解析失败，按未登录处理: %v", err)
		return
	}
	if cred.HasLogin() {
		logInfo("已接收父进程注入的登录凭证 musicid=%d", cred.MusicID)
		s.setCred(&cred)
	}
}

// Client 底层 API 客户端。
func (s *Session) Client() *qqmusic.Client { return s.cl }

// Mode 当前凭证模式。
func (s *Session) Mode() CredentialMode { return s.mode }

// Credential 当前凭证快照。
func (s *Session) Credential() *qqmusic.Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cred
}

// LoggedIn 是否已登录。
func (s *Session) LoggedIn() bool { return s.Credential().HasLogin() }

// Require 登录守卫：未登录返回错误。
func (s *Session) Require() (*qqmusic.Credential, error) {
	cred := s.Credential()
	if !cred.HasLogin() {
		return nil, qqmusic.NewCredentialInvalidError("需要登录：请先在应用内扫码登录")
	}
	return cred, nil
}

// SelfEuin 当前账号的加密 UIN / 字符串 UIN（收藏歌单等接口的主键）。
func (s *Session) SelfEuin() string {
	cred, err := s.Require()
	if err != nil {
		return ""
	}
	return cred.SelfEuin()
}

// Adopt 登录成功后写入新凭证（内存 + 交接给主进程）。
func (s *Session) Adopt(cred *qqmusic.Credential) {
	s.setCred(cred)
	if s.mode == ModeExternal {
		s.emitCredential(cred)
	}
	logInfo("登录凭证已更新 musicid=%d", cred.MusicID)
	s.notify()
}

// Logout 登出：上游注销 + 清本地凭证 + 交接 null。
func (s *Session) Logout() error {
	if s.LoggedIn() {
		if err := modulesLogin(s.cl).Logout(); err != nil {
			logWarn("上游登出失败，仅清除本地凭证: %v", err)
		}
	}
	if s.mode == ModeExternal {
		s.emitCredential(nil)
	}
	s.setCred(&qqmusic.Credential{})
	s.notify()
	return nil
}

func (s *Session) setCred(cred *qqmusic.Credential) {
	s.mu.Lock()
	s.cred = cred
	s.mu.Unlock()
	s.cl.SetCredential(cred)
}

// emitCredential 把凭证交给父进程加密落盘；null 表示登出。
//
// stdout 是 sidecar 的日志通道，主进程按前缀分流——这一行必须单行、必须立刻
// flush（stdout 走管道时是块缓冲），且绝不能同时写进 stderr/日志。
func (s *Session) emitCredential(cred *qqmusic.Credential) {
	payload := "null"
	if cred != nil {
		raw, _ := json.Marshal(cred)
		payload = string(raw)
	}
	s.mu.Lock()
	_, _ = s.out.WriteString(HandoffPrefix + payload + "\n")
	_ = s.out.Flush()
	s.mu.Unlock()
}

// OnCredentialChange 注册登录态变更回调（Typhoeus 清会员缓存等）。
func (s *Session) OnCredentialChange(cb func()) {
	s.listenersMu.Lock()
	s.listeners = append(s.listeners, cb)
	s.listenersMu.Unlock()
}

func (s *Session) notify() {
	s.listenersMu.Lock()
	cbs := append([]func(){}, s.listeners...)
	s.listenersMu.Unlock()
	for _, cb := range cbs {
		func() {
			defer func() { recover() }()
			cb()
		}()
	}
}

// modulesLogin 延迟构造 login 模块（避免 server→modules 的循环初始化）。
func modulesLogin(cl *qqmusic.Client) *modules.LoginModule {
	return modules.NewLoginModule(cl)
}
