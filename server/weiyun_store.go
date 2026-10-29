package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// weiyunAuth is the Weiyun Skill API Key. In Electron mode it is owned by the
// parent process and reaches the sidecar only through the encrypted handoff;
// the in-process copy below is the sole representation. The legacy plaintext
// migration is intentionally left to the parent process.
type weiyunAuth struct {
	Token  string `json:"token"`
	MCPURL string `json:"mcp_url,omitempty"`
}

type weiyunStore struct {
	mu       sync.RWMutex
	auth     *weiyunAuth
	external bool
}

func weiyunAuthFile() string {
	return filepath.Join(ConfigDir(), "weiyun-auth.json")
}

func (s *weiyunStore) load() *weiyunAuth {
	s.mu.RLock()
	if s.auth != nil {
		a := *s.auth
		s.mu.RUnlock()
		return &a
	}
	s.mu.RUnlock()

	raw, err := os.ReadFile(weiyunAuthFile())
	if err != nil {
		return nil
	}
	var a weiyunAuth
	if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.Token) == "" {
		return nil
	}
	s.mu.Lock()
	s.auth = &a
	s.mu.Unlock()
	out := a
	return &out
}

func (s *weiyunStore) save(token, mcpURL string) error {
	token = strings.TrimSpace(token)
	mcpURL = strings.TrimSpace(mcpURL)
	if token == "" {
		return errors.New("微云令牌不能为空")
	}
	if mcpURL == "" {
		mcpURL = "https://www.weiyun.com/api/v3/mcpserver"
	}
	a := &weiyunAuth{Token: token, MCPURL: mcpURL}
	s.mu.Lock()
	if s.external {
		s.auth = a
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := ensureCloudDirs(); err != nil {
		return err
	}
	raw, _ := json.Marshal(a)
	if err := writeCloudFile(weiyunAuthFile(), raw, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.auth = a
	s.mu.Unlock()
	return nil
}

// setExternal is called only while the sidecar bootstraps: the parent process is
// the owner of the encrypted store, so the token stays in memory here.
func (s *weiyunStore) setExternal(token, mcpURL string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	if strings.TrimSpace(mcpURL) == "" {
		mcpURL = "https://www.weiyun.com/api/v3/mcpserver"
	}
	s.mu.Lock()
	s.external = true
	s.auth = &weiyunAuth{Token: token, MCPURL: mcpURL}
	s.mu.Unlock()
}

func (s *weiyunStore) clear() error {
	s.mu.Lock()
	s.auth = nil
	external := s.external
	s.mu.Unlock()
	if external {
		return nil
	}
	err := os.Remove(weiyunAuthFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
