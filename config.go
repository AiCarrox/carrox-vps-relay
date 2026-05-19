// Per-user CF Token storage.
//
// Each user's CF API token is stored at /etc/vps-relay/users/{key_id}/cf-token.
package main

import (
	"os"
	"path/filepath"
	"strings"
)

const userDir = "/etc/vps-relay/users"

func cfTokenPath(keyID string) string {
	return filepath.Join(userDir, keyID, "cf-token")
}

func cfTokenForKey(keyID string) string {
	b, err := os.ReadFile(cfTokenPath(keyID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func saveCfTokenForKey(keyID, token string) error {
	dir := filepath.Join(userDir, keyID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := cfTokenPath(keyID)
	tmp, err := os.CreateTemp(dir, ".cf-token.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func maskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) <= 8 {
		return strings.Repeat("•", len(t))
	}
	return t[:4] + strings.Repeat("•", 8) + t[len(t)-4:]
}
