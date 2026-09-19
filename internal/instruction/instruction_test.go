package instruction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAndReadGlobalThenProjectInstructions(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(t.TempDir(), "shared-agents.md")
	if err := os.WriteFile(global, []byte("# Global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	documents := Discover(global, root, 1024)
	if len(documents) != 2 || documents[0].ID != "global" || documents[1].ID != "project" {
		t.Fatalf("documents: %+v", documents)
	}
	if documents[0].SHA256 == "" || documents[1].Bytes == 0 {
		t.Fatalf("missing document metadata: %+v", documents)
	}
	document, content, err := Read(global, root, "project", 1024)
	if err != nil || document.Scope != "project" || content != "# Project\n" {
		t.Fatalf("read: document=%+v content=%q err=%v", document, content, err)
	}
}

func TestDiscoverAtNestedDirectoryChain(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(t.TempDir(), "global.md")
	if err := os.WriteFile(global, []byte("# Global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "frontend", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend", "AGENTS.md"), []byte("# FE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend", "src", "AGENTS.md"), []byte("# SRC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docs := DiscoverAt(global, root, "frontend/src/views/Home.vue", 1024)
	if len(docs) != 4 {
		t.Fatalf("want 4 docs, got %d %+v", len(docs), docs)
	}
	if docs[0].Scope != "global" || docs[1].Scope != "project" || docs[2].ID != "dir:frontend" || docs[3].ID != "dir:frontend/src" {
		t.Fatalf("unexpected chain: %+v", docs)
	}
	// Backend anchor must not pick frontend rules.
	backend := DiscoverAt(global, root, "backend/main.go", 1024)
	for _, doc := range backend {
		if strings.HasPrefix(doc.ID, "dir:frontend") {
			t.Fatalf("backend anchor must not load frontend rules: %+v", backend)
		}
	}
}

func TestDiscoverSkipsSymlinkAndOversizedDocuments(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "AGENTS.md")
	if err := os.Symlink(target, symlink); err == nil {
		if documents := Discover("", root, 1024); len(documents) != 0 {
			t.Fatalf("symlink must not be exposed: %+v", documents)
		}
		if err := os.Remove(symlink); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	if documents := Discover("", root, 3); len(documents) != 0 {
		t.Fatalf("oversized document must not be exposed: %+v", documents)
	}
}
