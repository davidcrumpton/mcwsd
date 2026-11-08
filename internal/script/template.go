package script

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
)

// fetchOpts carries the optional authentication and signature settings for a
// host.fetch call, mirroring the former FreeBSD jail template downloader.
// Credentials are referenced by environment-variable name (never embedded),
// matching the native provisioner's posture.
type fetchOpts struct {
	username    string
	passwordEnv string
	header      string
	headerEnv   string
	publicKey   string
	sha256      string
}

func strOpt(opts map[string]interface{}, key string) string {
	if opts == nil {
		return ""
	}
	if v, ok := opts[key].(string); ok {
		return v
	}
	return ""
}

func parseFetchOpts(opts map[string]interface{}) fetchOpts {
	return fetchOpts{
		username:    strOpt(opts, "username"),
		passwordEnv: strOpt(opts, "password_env"),
		header:      strOpt(opts, "header"),
		headerEnv:   strOpt(opts, "header_env"),
		publicKey:   strOpt(opts, "public_key"),
		sha256:      strOpt(opts, "sha256"),
	}
}

// download retrieves source to dest, applying optional HTTP Basic or header
// authentication. Redirects are pinned to the original origin and capped,
// matching the native template downloader's safety posture. A download that
// yields an empty body — or exceeds limit when limit > 0 — is an error.
func download(ctx context.Context, source, dest string, opts fetchOpts, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if opts.username != "" {
		req.SetBasicAuth(opts.username, os.Getenv(opts.passwordEnv))
	}
	if opts.header != "" {
		req.Header.Set(opts.header, os.Getenv(opts.headerEnv))
	}
	client := &http.Client{
		Timeout: 30 * time.Minute,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 || next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host {
				return errors.New("redirects must stay on the same origin and use fewer than ten hops")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("fetch request failed; check connectivity, TLS, and same-origin redirects")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch returned HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("fetch open destination: %w", err)
	}
	reader := io.Reader(resp.Body)
	if limit > 0 {
		reader = io.LimitReader(reader, limit+1)
	}
	n, copyErr := io.Copy(f, reader)
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("fetch write: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("fetch close: %w", closeErr)
	}
	if n == 0 || (limit > 0 && n > limit) {
		return errors.New("fetch download is empty or exceeds its size limit")
	}
	return nil
}

// readTemplateKey loads and validates an OpenPGP public key, refusing private
// keys.
func readTemplateKey(path string) (openpgp.EntityList, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template public key: %w", err)
	}
	var keys openpgp.EntityList
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		keys, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(bytes.TrimSpace(data)))
	} else {
		keys, err = openpgp.ReadKeyRing(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("parse template public key: %w", err)
	}
	if len(keys) == 0 {
		return nil, errors.New("template public key file contains no keys")
	}
	for _, key := range keys {
		if key.PrivateKey != nil {
			return nil, errors.New("template verification requires public keys, not private keys")
		}
		for _, subkey := range key.Subkeys {
			if subkey.PrivateKey != nil {
				return nil, errors.New("template verification requires public keys, not private subkeys")
			}
		}
	}
	return keys, nil
}

// verifyTemplate verifies an armored or binary detached OpenPGP signature over
// archive using the public key at publicKey.
func verifyTemplate(archive, signature, publicKey string) error {
	keys, err := readTemplateKey(publicKey)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(signature)
	if err != nil {
		return fmt.Errorf("read template signature: %w", err)
	}
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open template archive: %w", err)
	}
	defer f.Close()
	config := &packet.Config{}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN PGP SIGNATURE-----")) {
		_, _, err = openpgp.VerifyArmoredDetachedSignature(keys, f, bytes.NewReader(bytes.TrimSpace(data)), config)
	} else {
		_, _, err = openpgp.VerifyDetachedSignature(keys, f, bytes.NewReader(data), config)
	}
	if err != nil {
		return fmt.Errorf("template OpenPGP signature verification failed: %w", err)
	}
	return nil
}
