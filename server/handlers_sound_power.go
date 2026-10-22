package server

import (
	"net/http"
	"strings"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// soundPowerReportBody 渲染层上报的一段真实播放时间。
//
// stream_token 只用于取回同一首已协商流的 vkey；拿不到时按 mid 重新取一次标准音质凭证。
// 前端永远拿不到 vkey，也无需知道 imusic_tj 的 XML 协议。
type soundPowerReportBody struct {
	StreamToken        string `json:"stream_token"`
	SongID             int64  `json:"song_id"`
	SongMid            string `json:"mid"`
	MediaMid           string `json:"media_mid"`
	AudioTimeMS        int64  `json:"audio_time_ms"`
	PlayDurationSecond int    `json:"play_duration_seconds"`
	SongType           int    `json:"song_type"`
	SingerID           int64  `json:"singer_id"`
	CoSinger           string `json:"co_singer"`
}

// handleSoundPowerReport POST /sound-power/report
func (a *App) handleSoundPowerReport(w http.ResponseWriter, r *http.Request) {
	var body soundPowerReportBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.SongID <= 0 || strings.TrimSpace(body.SongMid) == "" {
		logWarn("音响力上报失败: 缺少歌曲 id/mid")
		writeErr(w, http.StatusUnprocessableEntity, -1, "缺少歌曲 id/mid")
		return
	}
	if body.PlayDurationSecond <= 0 {
		logWarn("音响力上报失败: song=%d duration=%ds 参数非法", body.SongID, body.PlayDurationSecond)
		writeErr(w, http.StatusUnprocessableEntity, -1, "播放时长必须大于 0 秒")
		return
	}
	if body.AudioTimeMS <= 0 {
		logWarn("音响力上报失败: song=%d 缺少歌曲总时长", body.SongID)
		writeErr(w, http.StatusUnprocessableEntity, -1, "缺少歌曲总时长")
		return
	}
	if body.PlayDurationSecond > 3600 {
		body.PlayDurationSecond = 3600
	}

	ok, err := a.call2(func() (bool, error) {
		vkey := a.playbackVKey(body.StreamToken, body.SongMid)
		vkeySource := "stream"
		if vkey == "" {
			vkeySource = "fetch"
			var err error
			vkey, err = a.song.GetPlaybackVKey(body.SongMid, body.MediaMid)
			if err != nil {
				return false, err
			}
		}
		if vkey == "" {
			// 无凭证是失败，但属于确定性失败：告诉前端这一段没法报，不触发无意义的重试。
			logWarn("音响力上报失败: song=%d 无法获取 vkey", body.SongID)
			return false, nil
		}
		logSoundPowerAttempt(body, vkeySource)
		return a.session.Client().ReportPlayDuration(r.Context(), qqmusic.PlayDurationReport{
			SongID:             body.SongID,
			AudioTimeMS:        body.AudioTimeMS,
			PlayDurationSecond: body.PlayDurationSecond,
			VKey:               vkey,
			CoSinger:           body.CoSinger,
			SingerID:           body.SingerID,
		})
	})
	if err != nil {
		logWarn("音响力上报失败: song=%d mid=%s duration=%ds err=%v",
			body.SongID, body.SongMid, body.PlayDurationSecond, err)
		writeError(w, err)
		return
	}
	logSoundPowerResult(body, ok)
	payload := map[string]any{"ok": ok, "duration": body.PlayDurationSecond}
	if !ok {
		payload["reason"] = "upstream_rejected"
	}
	writeOK(w, payload)
}

// logSoundPowerAttempt 成功路径的完整流水只在 Crotchet 下输出，避免默认日志常驻刷屏。
func logSoundPowerAttempt(body soundPowerReportBody, vkeySource string) {
	if !crotchetEnabled() {
		return
	}
	logInfo("音响力上报: song=%d mid=%s duration=%ds audio_time=%dms vkey_source=%s",
		body.SongID, body.SongMid, body.PlayDurationSecond, body.AudioTimeMS, vkeySource)
}

// logSoundPowerResult 失败始终 WARN；成功结果只在 Crotchet 下输出。
func logSoundPowerResult(body soundPowerReportBody, ok bool) {
	if !ok {
		logWarn("音响力上报失败: song=%d duration=%ds 上游拒绝", body.SongID, body.PlayDurationSecond)
		return
	}
	if crotchetEnabled() {
		logInfo("音响力上报结果: song=%d duration=%ds ok=true", body.SongID, body.PlayDurationSecond)
	}
}

// playbackVKey 从仍在有效期内的播放 token 取回当次实际使用的 vkey。
func (a *App) playbackVKey(token, mid string) string {
	if token == "" {
		return ""
	}
	a.streamMu.Lock()
	entry := a.streams[token]
	a.streamMu.Unlock()
	if entry == nil || entry.mid != mid {
		return ""
	}
	return entry.vkey
}
