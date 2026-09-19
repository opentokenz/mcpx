package server

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestCompactFileRevisionUses80BitBase64URL(t *testing.T) {
	full := "sha256:" + strings.Repeat("ab", 32)
	rev := compactFileRevision(full)
	if len(rev) != 14 {
		t.Fatalf("compact revision length=%d value=%q, want 14 base64url chars", len(rev), rev)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rev)
	if err != nil {
		t.Fatalf("compact revision is not raw base64url: %q: %v", rev, err)
	}
	if len(raw) != compactFileRevisionBytes {
		t.Fatalf("compact revision bytes=%d, want %d", len(raw), compactFileRevisionBytes)
	}
	for _, value := range raw {
		if value != 0xab {
			t.Fatalf("compact revision did not preserve SHA prefix: %x", raw)
		}
	}
}
