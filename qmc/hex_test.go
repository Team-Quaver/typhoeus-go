package qmc

import "encoding/hex"

func mustHex(t interface{ Fatalf(string, ...any) }, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}
