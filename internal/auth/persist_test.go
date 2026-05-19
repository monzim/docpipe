package auth

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadOrGenerate_EnvWins(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"explicit": "ak_live_explicit"}
	keys, generated, err := LoadOrGenerate(dir, env, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Error("should NOT generate when env keys are present")
	}
	if len(keys) != 1 || keys["explicit"] != "ak_live_explicit" {
		t.Errorf("got %v want {explicit: ak_live_explicit}", keys)
	}
	if _, err := os.Stat(filepath.Join(dir, keysFile)); err == nil {
		t.Error("persisted file should not be written when env is set")
	}
}

func TestLoadOrGenerate_FirstRunGenerates(t *testing.T) {
	dir := t.TempDir()
	keys, generated, err := LoadOrGenerate(dir, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("first run should generate fresh keys")
	}
	if len(keys) != AutoGenCount {
		t.Errorf("got %d keys want %d", len(keys), AutoGenCount)
	}
	for name, secret := range keys {
		if !strings.HasPrefix(name, "key-") {
			t.Errorf("name %q should start with key-", name)
		}
		if !strings.HasPrefix(secret, keySecretPrefix) {
			t.Errorf("secret %q should start with %s", secret, keySecretPrefix)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, keysFile)); err != nil {
		t.Errorf("persisted file missing: %v", err)
	}
}

func TestLoadOrGenerate_RestartLoadsPersisted(t *testing.T) {
	dir := t.TempDir()

	// First run — generate.
	first, _, err := LoadOrGenerate(dir, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Second run — should reload, not regenerate.
	second, generated, err := LoadOrGenerate(dir, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Error("second run should reload from disk, not regenerate")
	}
	if len(first) != len(second) {
		t.Fatalf("len mismatch: first=%d second=%d", len(first), len(second))
	}
	for name, secret := range first {
		if second[name] != secret {
			t.Errorf("key %q: secret changed across restart", name)
		}
	}
}

func TestLoadOrGenerate_CorruptArchivesAndRegenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keysFile)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, generated, err := LoadOrGenerate(dir, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("corrupt file should trigger regeneration")
	}
	if len(keys) != AutoGenCount {
		t.Errorf("got %d keys want %d", len(keys), AutoGenCount)
	}
	// Broken file should be archived aside.
	entries, _ := os.ReadDir(dir)
	var hasBroken bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), keysFile+".broken.") {
			hasBroken = true
		}
	}
	if !hasBroken {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected %s.broken.<ts> archive; got %v", keysFile, names)
	}
}

func TestPersistedFilePermissions(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrGenerate(dir, nil, quietLogger()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, keysFile))
	if err != nil {
		t.Fatal(err)
	}
	// Secrets in plaintext — must be owner-only readable.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %o; want 0600", info.Mode().Perm())
	}
}

func TestRandomSecret_FormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := randomSecret()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, keySecretPrefix) {
			t.Errorf("missing prefix: %s", s)
		}
		if len(s) != len(keySecretPrefix)+64 {
			t.Errorf("unexpected length %d for %s", len(s), s)
		}
		if seen[s] {
			t.Errorf("duplicate secret on iteration %d", i)
		}
		seen[s] = true
	}
}
