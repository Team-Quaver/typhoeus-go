package server

import "testing"

func TestCrotchetEnabled(t *testing.T) {
	cases := map[string]bool{
		"": false, "0": false, "false": false, "FALSE": false,
		"1": true, "9300": true, "true": true,
	}
	for raw, want := range cases {
		t.Setenv("QUAVER_CROTCHET", raw)
		if got := crotchetEnabled(); got != want {
			t.Fatalf("QUAVER_CROTCHET=%q: got %v want %v", raw, got, want)
		}
	}
}
