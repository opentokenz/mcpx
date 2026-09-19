package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"mcpx/internal/edit"
	"mcpx/internal/file"
)

const compactFileRevisionBytes = 10 // 80 bits; model-facing only.

func compactFileRevision(fullSHA string) string {
	value := strings.ToLower(strings.TrimSpace(fullSHA))
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) < compactFileRevisionBytes*2 {
		return ""
	}
	raw, err := hex.DecodeString(value[:compactFileRevisionBytes*2])
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func sourceFileSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func compactRevisionPayload(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return value
	}
	rewriteRevisionFields(normalized)
	return normalized
}

func rewriteRevisionFields(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if full, ok := typed["sha256"].(string); ok {
			if rev := compactFileRevision(full); rev != "" {
				delete(typed, "sha256")
				typed["rev"] = rev
			}
		}
		for _, child := range typed {
			rewriteRevisionFields(child)
		}
	case []any:
		for _, child := range typed {
			rewriteRevisionFields(child)
		}
	}
}

func resolveEditRevisions(workspaceRoot string, edits []edit.FileEdit) error {
	for i := range edits {
		op := strings.TrimSpace(edits[i].Operation)
		if op != edit.OpUpdate && op != edit.OpRename {
			continue
		}
		rev := strings.TrimSpace(edits[i].Revision)
		if rev == "" {
			continue
		}
		absolute, err := file.Resolve(workspaceRoot, edits[i].Path)
		if err != nil {
			return &edit.ApplyError{Code: "NOT_FOUND", Message: err.Error(), Path: edits[i].Path, Index: i, Err: err}
		}
		content, err := os.ReadFile(absolute)
		if err != nil {
			return &edit.ApplyError{Code: "NOT_FOUND", Message: err.Error(), Path: edits[i].Path, Index: i, Err: err}
		}
		actual := sourceFileSHA256(content)
		if compactFileRevision(actual) != rev {
			return &edit.ApplyError{Code: "STALE_REVISION", Message: "rev does not match current file", Path: edits[i].Path, Index: i, Current: actual, Err: edit.ErrStale}
		}
		edits[i].BaseSHA256 = actual
		edits[i].Revision = ""
	}
	return nil
}
