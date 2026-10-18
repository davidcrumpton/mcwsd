package freebsd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/davidcrumpton/mcwsd/internal"
)

// TemplateOptions selects an administrator-built root archive. Credentials
// are read from environment variables, never embedded in URLs or command lines.
type TemplateOptions struct {
	BaseURL         string
	Username        string
	PasswordEnv     string
	Header          string
	HeaderEnv       string
	VerifySignature bool
	PublicKey       string
}

func (p *FreeBSDJailProvisioner) WithTemplate(options TemplateOptions) *FreeBSDJailProvisioner {
	p.template = options
	return p
}

func (p *FreeBSDJailProvisioner) validateTemplate() error {
	t := p.template
	if t.BaseURL == "" {
		if t.Username != "" || t.PasswordEnv != "" || t.Header != "" || t.HeaderEnv != "" || t.VerifySignature || t.PublicKey != "" {
			return errors.New("template authentication and signature options require base-template-url")
		}
		return nil
	}
	u, err := url.Parse(t.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base-template-url must be an HTTP(S) directory URL without credentials, query, or fragment")
	}
	if err := validateRelease(p.release); err != nil {
		return err
	}
	if p.release == "." || p.release == ".." {
		return errors.New("template release must be a filename")
	}
	if (t.Username == "") != (t.PasswordEnv == "") {
		return errors.New("template Basic authentication requires both user and password-env")
	}
	if (t.Header == "") != (t.HeaderEnv == "") {
		return errors.New("template header authentication requires both header and header-env")
	}
	if t.Username != "" && t.Header != "" {
		return errors.New("choose template Basic authentication or header authentication, not both")
	}
	if t.Header != "" {
		for _, c := range t.Header {
			if !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", c) {
				return errors.New("invalid template HTTP header name")
			}
		}
		switch strings.ToLower(t.Header) {
		case "host", "content-length", "transfer-encoding", "connection":
			return errors.New("template authentication header cannot control HTTP routing or framing")
		}
	}
	for _, name := range []string{t.PasswordEnv, t.HeaderEnv} {
		if name == "" {
			continue
		}
		value, ok := os.LookupEnv(name)
		if !ok || value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("template credential environment variable %s is missing, empty, or contains a newline", name)
		}
	}
	if t.VerifySignature {
		if t.PublicKey == "" {
			return errors.New("verify-signature requires template-public-key")
		}
		if _, err := readTemplateKey(t.PublicKey); err != nil {
			return err
		}
	} else if t.PublicKey != "" {
		return errors.New("template-public-key requires verify-signature")
	}
	return nil
}

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

func (p *FreeBSDJailProvisioner) verifyTemplate(archive, signature string) error {
	keys, err := readTemplateKey(p.template.PublicKey)
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

func (p *FreeBSDJailProvisioner) ensureTemplateArchive(ctx context.Context, filename string) (string, error) {
	p.templateMu.Lock()
	defer p.templateMu.Unlock()
	if err := p.validateTemplate(); err != nil {
		return "", err
	}
	if err := validateRelease(filename); err != nil || filename == "." || filename == ".." {
		return "", errors.New("template release must be a safe filename")
	}
	source, err := url.JoinPath(p.template.BaseURL, filename)
	if err != nil {
		return "", errors.New("cannot construct template archive URL")
	}
	digest := sha256.Sum256([]byte(source))
	dest := filepath.Join(p.baseJailPath, "media", "templates", fmt.Sprintf("%x", digest), filename)
	if info, err := os.Stat(dest); err == nil {
		if info.Size() == 0 || !info.Mode().IsRegular() {
			return "", errors.New("cached template archive is empty or not a regular file")
		}
		internal.Log.Debug("template_archive_cache_hit", "release", filename, "path", dest, "size_bytes", info.Size())
		if p.template.VerifySignature {
			if err := p.verifyTemplate(dest, dest+".sig"); err != nil {
				return "", err
			}
		}
		return dest, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect cached template: %w", err)
	}
	internal.Log.Info("template_archive_download_started", "release", filename, "signature_verification", p.template.VerifySignature)
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", fmt.Errorf("create template cache: %w", err)
	}
	archive, err := os.CreateTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return "", fmt.Errorf("create template download file: %w", err)
	}
	tmp := archive.Name()
	defer os.Remove(tmp)
	if err := archive.Close(); err != nil {
		return "", err
	}
	if err := p.downloadTemplate(ctx, source, tmp, 0); err != nil {
		return "", err
	}
	if p.template.VerifySignature {
		sig := tmp + ".sig"
		defer os.Remove(sig)
		if err := p.downloadTemplate(ctx, source+".sig", sig, 1<<20); err != nil {
			return "", err
		}
		if err := p.verifyTemplate(tmp, sig); err != nil {
			return "", err
		}
		internal.Log.Info("template_signature_verified", "release", filename)
		if err := os.Rename(sig, dest+".sig"); err != nil {
			return "", fmt.Errorf("cache template signature: %w", err)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("cache template archive: %w", err)
	}
	internal.Log.Info("template_archive_cached", "release", filename)
	return dest, nil
}

func (p *FreeBSDJailProvisioner) downloadTemplate(ctx context.Context, source, dest string, limit int64) error {
	artifact := "archive"
	if limit > 0 {
		artifact = "signature"
	}
	internal.Log.Debug("template_download_started", "artifact", artifact)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return errors.New("cannot construct template download request")
	}
	if p.template.Username != "" {
		req.SetBasicAuth(p.template.Username, os.Getenv(p.template.PasswordEnv))
	}
	if p.template.Header != "" {
		req.Header.Set(p.template.Header, os.Getenv(p.template.HeaderEnv))
	}
	client := &http.Client{
		Timeout: 30 * time.Minute,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 || next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host {
				return errors.New("template redirects must stay on the same origin and use fewer than ten hops")
			}
			return nil
		},
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("template download cancelled: %w", ctx.Err())
		}
		// Transport errors may contain redirect URLs or credential values.
		return errors.New("template HTTP request failed; check connectivity, TLS, and same-origin redirects")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("template download returned HTTP %d", response.StatusCode)
	}
	internal.Log.Debug("template_download_response", "artifact", artifact, "status_code", response.StatusCode)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open template download destination: %w", err)
	}
	reader := io.Reader(response.Body)
	if limit > 0 {
		reader = io.LimitReader(reader, limit+1)
	}
	n, copyErr := io.Copy(f, reader)
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("write template download: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close template download: %w", closeErr)
	}
	if n == 0 || (limit > 0 && n > limit) {
		return errors.New("template download is empty or exceeds its size limit")
	}
	internal.Log.Debug("template_download_complete", "artifact", artifact, "size_bytes", n)
	return nil
}
