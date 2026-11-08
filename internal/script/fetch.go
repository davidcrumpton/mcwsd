package script

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// verifySHA256 checks that the file at path matches the expected hex digest.
func verifySHA256(path, expected string) error {
	expected = strings.TrimSpace(strings.ToLower(expected))
	if expected == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != expected {
		return fmt.Errorf("checksum mismatch: want %s, got %s", expected, got)
	}
	return nil
}
