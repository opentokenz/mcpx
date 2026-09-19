package audit

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestLogNoPassword(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Log(Event{
		RequestID:       "r1",
		RemoteSessionID: "rs1",
		Tool:            "terminal_exec",
		Command:         "ssh host",
		Status:          "ok",
		HasPassword:     true,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("audit mode %o, want 600", got)
	}
	s := string(b)
	if strings.Contains(s, `"password"`) && strings.Contains(s, "secretvalue") {
		t.Fatal("must not log secrets")
	}
	if !strings.Contains(s, `"has_password":true`) {
		t.Fatalf("body %s", s)
	}
	if !strings.Contains(s, `"remote_session_id":"rs1"`) || strings.Contains(s, `"session_id"`) {
		t.Fatalf("body %s", s)
	}
}
