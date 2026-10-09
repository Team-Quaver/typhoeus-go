//go:build linux

package inhibit

import "testing"

func TestLinuxBackendModeMapping(t *testing.T) {
	tests := []struct {
		name       string
		mode       Mode
		portalFlag uint32
		logindWhat string
		reason     string
	}{
		{"sleep", ModeSleep, inhibitSuspend, "sleep", "Quaver 正在播放音频"},
		{"idle", ModeIdle, inhibitIdle, "idle", "Quaver 正在画廊播放"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &linuxBackend{mode: tt.mode}
			if got := b.portalFlags(); got != tt.portalFlag {
				t.Errorf("portalFlags() = %#x, want %#x", got, tt.portalFlag)
			}
			if got := b.logindWhat(); got != tt.logindWhat {
				t.Errorf("logindWhat() = %q, want %q", got, tt.logindWhat)
			}
			if got := b.reason(); got != tt.reason {
				t.Errorf("reason() = %q, want %q", got, tt.reason)
			}
		})
	}
}
