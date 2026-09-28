package qqmusic

import (
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"
)

// AndroidSession 是 Android 设备的匿名会话（uid/sid），对标 utils/android_session.py。
//
// 首个业务请求前需向 music.getSession.session/GetSession 领取一次；
// 会话按「自然日」保活：跨天后下一次请求会以 caller=1 续期。
type AndroidSession struct {
	UID     string
	SID     string
	SavedAt int64
}

// savedToday 会话是否在今天领取/续期。
func (s *AndroidSession) savedToday() bool {
	return time.Unix(s.SavedAt, 0).Format("2006-01-02") == time.Now().Format("2006-01-02")
}

// ensureAndroidSession 取会话（命中内存/磁盘缓存且未跨天则直接复用）。
func (cl *Client) ensureAndroidSession() (*AndroidSession, error) {
	cl.sessionMu.Lock()
	defer cl.sessionMu.Unlock()
	if cl.androidSession != nil && cl.androidSession.savedToday() {
		return cl.androidSession, nil
	}
	// 磁盘缓存
	if cl.androidSession == nil && cl.cachePath != "" {
		if s := readSessionCache(cl.cachePath); s != nil && s.savedToday() {
			cl.androidSession = s
			return s, nil
		}
	}
	caller := 1
	var staleUID string
	if cl.androidSession != nil {
		staleUID = cl.androidSession.UID
	} else {
		caller = 2
	}
	s, err := cl.refreshAndroidSession(staleUID, caller)
	if err != nil {
		return nil, err
	}
	cl.androidSession = s
	if cl.cachePath != "" {
		writeSessionCache(cl.cachePath, s)
	}
	return s, nil
}

// refreshAndroidSession 发起 GetSession 请求并校验 uid/sid。
func (cl *Client) refreshAndroidSession(staleUID string, caller int) (*AndroidSession, error) {
	comm := cl.BuildComm(PlatformAndroid, &Credential{})
	q, err := cl.qimeiCached()
	if err == nil && q != nil {
		comm.Set("QIMEI36", q.Q36)
	}
	param := NewJObj().
		Set("uid", staleUID).
		Set("vkey", 0).
		Set("caller", caller)
	payload := NewJObj().
		Set("comm", comm).
		Set("req_0", NewJObj().
			Set("module", "music.getSession.session").
			Set("method", "GetSession").
			Set("param", param))
	req, err := newRequest("POST", musicuURL, payload.Marshal())
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", cl.UserAgent(PlatformAndroid))
	resp, body, err := cl.doRequest(req, 15*time.Second, false)
	if err != nil {
		return nil, err
	}
	_ = resp
	data, err := unwrapCGIResponse(body, 0)
	if err != nil {
		return nil, err
	}
	var sess struct {
		Session struct {
			UID any    `json:"uid"`
			SID string `json:"sid"`
		} `json:"session"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, errData("Android Session 响应格式异常: " + err.Error())
	}
	uid := ""
	switch x := sess.Session.UID.(type) {
	case string:
		uid = x
	case float64:
		uid = strconv.FormatInt(int64(x), 10)
	}
	if uid == "" || sess.Session.SID == "" {
		return nil, errData("Android Session 响应缺少有效的 uid/sid")
	}
	return &AndroidSession{UID: uid, SID: sess.Session.SID, SavedAt: time.Now().Unix()}, nil
}

// ---- 会话缓存（device.cache.json 的 session 槽）----

type diskCache struct {
	Qimei   *qimeiCacheEntry `json:"qimei,omitempty"`
	Session *sessCacheEntry  `json:"session,omitempty"`
}

type qimeiCacheEntry struct {
	Q16     string `json:"q16"`
	Q36     string `json:"q36"`
	SavedAt int64  `json:"saved_at"`
}

type sessCacheEntry struct {
	UID     string `json:"uid"`
	SID     string `json:"sid"`
	SavedAt int64  `json:"saved_at"`
}

var cacheFileMu sync.Mutex

func readDiskCache(path string) *diskCache {
	cacheFileMu.Lock()
	defer cacheFileMu.Unlock()
	raw, err := readFileLimited(path)
	if err != nil {
		return &diskCache{}
	}
	var c diskCache
	if json.Unmarshal(raw, &c) != nil {
		return &diskCache{}
	}
	return &c
}

func writeDiskCache(path string, mutate func(*diskCache)) {
	cacheFileMu.Lock()
	defer cacheFileMu.Unlock()
	c := &diskCache{}
	if raw, err := readFileLimited(path); err == nil {
		_ = json.Unmarshal(raw, c)
	}
	mutate(c)
	if raw, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(path, raw, 0o600)
	}
}

func readSessionCache(path string) *AndroidSession {
	c := readDiskCache(path)
	if c.Session == nil || c.Session.UID == "" || c.Session.SID == "" {
		return nil
	}
	return &AndroidSession{UID: c.Session.UID, SID: c.Session.SID, SavedAt: c.Session.SavedAt}
}

func writeSessionCache(path string, s *AndroidSession) {
	writeDiskCache(path, func(c *diskCache) {
		c.Session = &sessCacheEntry{UID: s.UID, SID: s.SID, SavedAt: s.SavedAt}
	})
}

func readQimeiCache(path string) *qimeiCacheEntry {
	c := readDiskCache(path)
	return c.Qimei
}

func writeQimeiCache(path string, q *qimeiResult) {
	writeDiskCache(path, func(c *diskCache) {
		c.Qimei = &qimeiCacheEntry{Q16: q.Q16, Q36: q.Q36, SavedAt: time.Now().Unix()}
	})
}

// readFileLimited 读小文件（缓存文件都在 KB 量级）。
func readFileLimited(path string) ([]byte, error) {
	return os.ReadFile(path)
}
