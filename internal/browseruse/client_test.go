package browseruse

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestIDAcceptsStringAndNumber(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want ID
	}{
		{raw: `123`, want: "123"},
		{raw: `"abc"`, want: "abc"},
		{raw: `null`, want: ""},
	} {
		var got ID
		if err := json.Unmarshal([]byte(test.raw), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", test.raw, err)
		}
		if got != test.want {
			t.Fatalf("Unmarshal(%s)=%q, want %q", test.raw, got, test.want)
		}
	}
}

func TestFrameRoundTrip(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":"pong"}`)
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, payload); err != nil {
		t.Fatal(err)
	}
	got, err := readFrame(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("frame payload=%q, want %q", got, payload)
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	err := writeFrame(&bytes.Buffer{}, []byte(strings.Repeat("x", maxFrameBytes+1)))
	if err == nil {
		t.Fatal("expected oversize frame error")
	}
}
