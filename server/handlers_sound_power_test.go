package server

import (
	"strings"
	"testing"
)

func TestSoundPowerLogsOnlySuccessInCrotchet(t *testing.T) {
	body := soundPowerReportBody{
		SongID: 735018740, SongMid: "004Bjrow3AzR86",
		PlayDurationSecond: 60, AudioTimeMS: 208000,
	}
	t.Setenv("QUAVER_CROTCHET", "")
	out := captureStderr(t, func() {
		logSoundPowerAttempt(body, "stream")
		logSoundPowerResult(body, true)
	})
	if out != "" {
		t.Fatalf("default mode must stay quiet on success: %q", out)
	}

	out = captureStderr(t, func() { logSoundPowerResult(body, false) })
	if !strings.Contains(out, "音响力上报失败") || !strings.Contains(out, "上游拒绝") {
		t.Fatalf("upstream rejection must always warn: %q", out)
	}

	t.Setenv("QUAVER_CROTCHET", "1")
	out = captureStderr(t, func() {
		logSoundPowerAttempt(body, "stream")
		logSoundPowerResult(body, true)
	})
	for _, want := range []string{"vkey_source=stream", "audio_time=208000ms", "ok=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("crotchet log missing %q: %q", want, out)
		}
	}
}
