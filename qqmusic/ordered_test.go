package qqmusic

import (
	"encoding/json"
	"testing"
)

func TestJObjMarshalsJSONNumber(t *testing.T) {
	got := string(NewJObj().Set("song_id", json.Number("735018740")).Marshal())
	if got != `{"song_id":735018740}` {
		t.Fatalf("marshal = %s", got)
	}
}
