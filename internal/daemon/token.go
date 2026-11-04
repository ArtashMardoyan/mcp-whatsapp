package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadOrCreateToken returns the bearer token stored at path, creating it with
// 32 random bytes (hex) and mode 0600 on first use. The token is never logged.
func LoadOrCreateToken(path string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		tok := hex.EncodeToString(b)
		_, werr := f.WriteString(tok + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", fmt.Errorf("write token file: %w", werr)
		}
		return tok, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create token file: %w", err)
	}
	// Existing file: tighten permissions in case it was copied or edited.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod token file: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if len(tok) < 16 {
		return "", fmt.Errorf("token file %s is empty or too short; delete it to regenerate", path)
	}
	return tok, nil
}
