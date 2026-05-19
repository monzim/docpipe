package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// keysFile is the on-disk filename inside DataDir.
const keysFile = "api-keys.json"

// keySecretPrefix matches the convention from scripts/gen-apikey.sh so the
// auto-generated keys are visually indistinguishable from human-generated ones.
const keySecretPrefix = "ak_live_"

// AutoGenCount is the number of keys generated when no DOCPIPE_API_KEYS env
// is set and no persisted file exists. Spec says minimum 10.
const AutoGenCount = 10

// PersistedKeys is the on-disk shape. We keep secrets in plaintext because
// the user must be able to recover them to use them. The file lives inside
// DOCPIPE_DATA_DIR, which is typically a non-public volume.
type PersistedKeys struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Keys        map[string]string `json:"keys"` // name → secret
}

// LoadOrGenerate resolves the active key set in this priority order:
//
//  1. If `envKeys` (parsed from DOCPIPE_API_KEYS) is non-empty, use it as-is.
//     The persisted file is left untouched — explicit env always wins.
//  2. Otherwise, if `${dataDir}/api-keys.json` exists, load it.
//  3. Otherwise, generate `AutoGenCount` keys, persist them, and return them.
//
// The boolean return reports whether fresh keys were generated this call
// (so the caller can emit a one-time "here are your new keys" log).
func LoadOrGenerate(dataDir string, envKeys map[string]string, log *slog.Logger) (map[string]string, bool, error) {
	if len(envKeys) > 0 {
		log.Info("apikeys_source",
			"source", "env",
			"count", len(envKeys),
			"names", sortedNames(envKeys),
		)
		return envKeys, false, nil
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, false, fmt.Errorf("create data dir: %w", err)
	}

	path := filepath.Join(dataDir, keysFile)
	if loaded, err := loadKeysFile(path); err == nil {
		log.Info("apikeys_source",
			"source", "persisted",
			"path", path,
			"count", len(loaded),
			"names", sortedNames(loaded),
		)
		return loaded, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Corrupt or unreadable — archive and regenerate so the service stays usable.
		log.Error("apikeys_persisted_corrupt", "path", path, "err", err)
		_ = os.Rename(path, fmt.Sprintf("%s.broken.%d", path, time.Now().Unix()))
	}

	fresh, err := generateKeys(AutoGenCount)
	if err != nil {
		return nil, false, fmt.Errorf("generate keys: %w", err)
	}
	if err := saveKeysFile(path, fresh); err != nil {
		return nil, false, fmt.Errorf("persist keys: %w", err)
	}
	log.Warn("apikeys_generated",
		"path", path,
		"count", len(fresh),
		"note", "fresh keys auto-generated; saved to disk for restart survival",
	)
	return fresh, true, nil
}

func loadKeysFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pk PersistedKeys
	if err := json.Unmarshal(data, &pk); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(pk.Keys) == 0 {
		return nil, errors.New("empty key set")
	}
	return pk.Keys, nil
}

func saveKeysFile(path string, keys map[string]string) error {
	pk := PersistedKeys{
		GeneratedAt: time.Now().UTC(),
		Keys:        keys,
	}
	data, err := json.MarshalIndent(pk, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// Restrict to owner-read-only — secrets in plaintext.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func generateKeys(n int) (map[string]string, error) {
	out := make(map[string]string, n)
	for i := 1; i <= n; i++ {
		secret, err := randomSecret()
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("key-%02d", i)
		out[name] = secret
	}
	return out, nil
}

// randomSecret returns `ak_live_` + 64 hex chars from 32 bytes of OS randomness.
func randomSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return keySecretPrefix + hex.EncodeToString(buf[:]), nil
}

func sortedNames(keys map[string]string) []string {
	names := make([]string, 0, len(keys))
	for n := range keys {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
