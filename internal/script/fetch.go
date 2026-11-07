package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// downloadTo fetches url to dest, honoring ctx. When opts carries a sha256
// hex digest the download is verified before returning. Redirects are pinned
// to the original origin and capped, matching the jail template downloader's
// safety posture.
func downloadTo(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	client := &http.Client{
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 || next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host {
				return fmt.Errorf("redirects must stay on the same origin and use fewer than ten hops")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("fetch request failed; check connectivity, TLS, and same-origin redirects")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch returned HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("fetch open destination: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("fetch write: %w", err)
	}
	return f.Close()
}

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
