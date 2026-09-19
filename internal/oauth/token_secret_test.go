package oauth

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreateTokenSecretPersistsSecureKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token-secret")
	first, err := LoadOrCreateTokenSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != tokenSecretBytes {
		t.Fatalf("secret length = %d", len(first))
	}
	second, err := LoadOrCreateTokenSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("secret changed between loads")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hex.DecodeString(string(encoded[:len(encoded)-1])); err != nil {
		t.Fatalf("secret file is not hex: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("secret permissions = %o", info.Mode().Perm())
	}
}

func TestLoadOrCreateTokenSecretRejectsInvalidExistingFiles(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":  "",
		"short":  "00",
		"odd":    "abc",
		"nonhex": "zzzz",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oauth-token-secret")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreateTokenSecret(path); err == nil {
				t.Fatal("expected invalid secret error")
			}
		})
	}
}

func TestLoadOrCreateTokenSecretConcurrentFirstUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token-secret")
	secrets := make([][]byte, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	for index := range secrets {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			secrets[index], errs[index] = LoadOrCreateTokenSecret(path)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", index, err)
		}
	}
	if string(secrets[0]) != string(secrets[1]) {
		t.Fatal("concurrent callers received different secrets")
	}
}

func TestLoadOrCreateTokenSecretRequiresPath(t *testing.T) {
	if _, err := LoadOrCreateTokenSecret(""); err == nil {
		t.Fatal("expected missing path error")
	}
}
