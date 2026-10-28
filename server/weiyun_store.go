package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// weiyunAuth is deliberately stored separately from QQ Music's credential.enc:
// it is a Weiyun MCP token, not a QQ Music login credential, and has a different
// lifetime/revocation flow. The file is 0600 and only the sidecar reads it.
type weiyunAuth struct {
	Token  string `json:"token"`
	MCPURL string `json:"mcp_url,omitempty"`
}

type weiyunStore struct {
	mu   sync.RWMutex
	auth *weiyunAuth
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

func (s *weiyunStore) clear() error {
	s.mu.Lock()
	s.auth = nil
	s.mu.Unlock()
	err := os.Remove(weiyunAuthFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
