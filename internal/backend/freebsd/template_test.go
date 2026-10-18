package freebsd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func templateSigningKey(t *testing.T, config *packet.Config) *openpgp.Entity {
	t.Helper()
	if config == nil {
		config = &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA}
	}
	key, err := openpgp.NewEntity("Template test", "", "template@example.invalid", config)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func templatePublicKey(t *testing.T, key *openpgp.Entity, armored bool) string {
	t.Helper()
	var data bytes.Buffer
	if armored {
		w, err := armor.Encode(&data, openpgp.PublicKeyType, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := key.Serialize(w); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else if err := key.Serialize(&data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "template.pub")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func templateSignature(t *testing.T, key *openpgp.Entity, payload []byte, armored bool, config *packet.Config) []byte {
	t.Helper()
	var sig bytes.Buffer
	var err error
	if armored {
		err = openpgp.ArmoredDetachSign(&sig, []*openpgp.Entity{key}, bytes.NewReader(payload), &openpgp.SignParams{Config: config})
	} else {
		err = openpgp.DetachSign(&sig, []*openpgp.Entity{key}, bytes.NewReader(payload), config)
	}
	if err != nil {
		t.Fatal(err)
	}
	return sig.Bytes()
}

func TestTemplateSignatureVerification(t *testing.T) {
	payload := []byte("organization jail archive")
	key := templateSigningKey(t, nil)
	unknown := templateSigningKey(t, nil)
	past := time.Now().Add(-time.Hour)
	expiringKey := templateSigningKey(t, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return past }})
	for _, tc := range []struct {
		name    string
		sig     []byte
		key     *openpgp.Entity
		payload []byte
		armored bool
		wantErr bool
	}{
		{"binary", templateSignature(t, key, payload, false, nil), key, payload, false, false},
		{"armored", templateSignature(t, key, payload, true, nil), key, payload, true, false},
		{"tampered archive", templateSignature(t, key, payload, false, nil), key, []byte("tampered"), false, true},
		{"unknown signer", templateSignature(t, unknown, payload, false, nil), key, payload, false, true},
		{"malformed", []byte("not a signature"), key, payload, false, true},
		{"empty", nil, key, payload, false, true},
		{"expired signature", templateSignature(t, expiringKey, payload, false, &packet.Config{Time: func() time.Time { return past }, SigLifetimeSecs: 1}), expiringKey, payload, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _, _, _ := newTestProvisioner(t)
			p.template.PublicKey = templatePublicKey(t, tc.key, tc.armored)
			archive := filepath.Join(t.TempDir(), "jail.tgz")
			if err := os.WriteFile(archive, tc.payload, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(archive+".sig", tc.sig, 0600); err != nil {
				t.Fatal(err)
			}
			err := p.verifyTemplate(archive, archive+".sig")
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyTemplate = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestTemplateRevokedKey(t *testing.T) {
	key := templateSigningKey(t, nil)
	payload := []byte("archive")
	sig := templateSignature(t, key, payload, false, nil)
	if err := key.Revoke(packet.KeyCompromised, "test", nil); err != nil {
		t.Fatal(err)
	}
	p, _, _, _, _ := newTestProvisioner(t)
	p.template.PublicKey = templatePublicKey(t, key, false)
	path := filepath.Join(t.TempDir(), "jail.tgz")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sig", sig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.verifyTemplate(path, path+".sig"); err == nil {
		t.Fatal("accepted a revoked signing key")
	}
}

func TestTemplateDownloadAuthAndCache(t *testing.T) {
	payload := []byte("signed jail archive")
	key := templateSigningKey(t, nil)
	sig := templateSignature(t, key, payload, true, nil)
	for _, auth := range []string{"anonymous", "basic", "header"} {
		t.Run(auth, func(t *testing.T) {
			t.Setenv("TEMPLATE_PASSWORD", "password")
			t.Setenv("TEMPLATE_HEADER", "Bearer token")
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if auth == "basic" {
					user, pass, ok := r.BasicAuth()
					if !ok || user != "admin" || pass != "password" {
						t.Error("Basic credentials missing or incorrect")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
				if auth == "header" && r.Header.Get("Authorization") != "Bearer token" {
					t.Error("header credentials missing or incorrect")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/templates/jail.tgz":
					_, _ = w.Write(payload)
				case "/templates/jail.tgz.sig":
					_, _ = w.Write(sig)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			p, _, downloader, _, _ := newTestProvisioner(t)
			p.release = "jail.tgz"
			p.WithTemplate(TemplateOptions{BaseURL: server.URL + "/templates", VerifySignature: true, PublicKey: templatePublicKey(t, key, true)})
			switch auth {
			case "basic":
				p.template.Username, p.template.PasswordEnv = "admin", "TEMPLATE_PASSWORD"
			case "header":
				p.template.Header, p.template.HeaderEnv = "Authorization", "TEMPLATE_HEADER"
			}
			first, err := p.ensureBaseArchive(context.Background(), p.release)
			if err != nil {
				t.Fatal(err)
			}
			second, err := p.ensureBaseArchive(context.Background(), p.release)
			if err != nil || first != second {
				t.Fatalf("cache reuse = %q, %v", second, err)
			}
			if downloader.count() != 0 {
				t.Fatal("custom template used official release downloader")
			}
			mu.Lock()
			count := len(paths)
			mu.Unlock()
			if count != 2 {
				t.Fatalf("got %d requests, want archive + signature only", count)
			}
			if err := os.WriteFile(first, []byte("cache tampering"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ensureBaseArchive(context.Background(), p.release); err == nil {
				t.Fatal("tampered cache accepted")
			}
			if err := os.Remove(first + ".sig"); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ensureBaseArchive(context.Background(), p.release); err == nil {
				t.Fatal("missing cached signature accepted")
			}
		})
	}
}

func TestTemplateRejectsBeforeExtraction(t *testing.T) {
	key := templateSigningKey(t, nil)
	for _, tc := range []struct {
		name    string
		status  int
		archive []byte
		sig     []byte
	}{
		{"missing signature", http.StatusNotFound, []byte("archive"), nil},
		{"bad signature", http.StatusOK, []byte("archive"), []byte("bad signature")},
		{"oversize signature", http.StatusOK, []byte("archive"), bytes.Repeat([]byte("x"), (1<<20)+1)},
		{"empty archive", http.StatusOK, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".sig") {
					w.WriteHeader(tc.status)
					_, _ = w.Write(tc.sig)
				} else {
					_, _ = w.Write(tc.archive)
				}
			}))
			defer server.Close()
			p, runner, _, _, rec := newTestProvisioner(t)
			p.release = "jail.tgz"
			p.WithTemplate(TemplateOptions{BaseURL: server.URL, VerifySignature: true, PublicKey: templatePublicKey(t, key, false)})
			_, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "rejected"})
			if err == nil {
				t.Fatal("invalid template accepted")
			}
			if rec.indexOf("extract") != -1 || runner.calledWith("jail -c") {
				t.Fatal("extracted or started an unverified jail")
			}
			var files []string
			if err := filepath.WalkDir(p.baseJailPath, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					files = append(files, path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("failed download left files: %v", files)
			}
		})
	}
}

func TestTemplateUnsignedAndSourceIsolation(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)
	p.release = "jail.tgz"
	var downloads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		if strings.HasSuffix(r.URL.Path, ".sig") {
			t.Error("unsigned template requested a signature")
		}
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	defer server.Close()
	p.WithTemplate(TemplateOptions{BaseURL: server.URL + "/one"})
	first, err := p.ensureBaseArchive(context.Background(), p.release)
	if err != nil {
		t.Fatal(err)
	}
	p.template.BaseURL = server.URL + "/two"
	second, err := p.ensureBaseArchive(context.Background(), p.release)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || downloads != 2 {
		t.Fatal("different template sources shared cache")
	}
}

func TestTemplateConfigurationValidation(t *testing.T) {
	for _, tc := range []TemplateOptions{
		{VerifySignature: true},
		{BaseURL: "file:///tmp"},
		{BaseURL: "https://user:secret@example.invalid/templates"},
		{BaseURL: "https://example.invalid/templates?token=secret"},
		{BaseURL: "https://example.invalid", VerifySignature: true},
		{BaseURL: "https://example.invalid", PublicKey: "/missing"},
		{BaseURL: "https://example.invalid", Username: "user"},
		{BaseURL: "https://example.invalid", Header: "Authorization"},
		{BaseURL: "https://example.invalid", Header: "Bad Header", HeaderEnv: "SECRET"},
		{BaseURL: "https://example.invalid", Header: "Host", HeaderEnv: "SECRET"},
		{BaseURL: "https://example.invalid", Header: "Authorization", HeaderEnv: "MISSING_TEMPLATE_SECRET"},
		{BaseURL: "https://example.invalid", VerifySignature: true, PublicKey: "/missing"},
	} {
		p, _, _, _, _ := newTestProvisioner(t)
		p.WithTemplate(tc)
		if err := p.Init(context.Background()); err == nil {
			t.Fatalf("invalid template configuration accepted: %+v", tc)
		}
	}
	for _, filename := range []string{".", "..", "../jail.tgz", "/jail.tgz"} {
		p, _, _, _, _ := newTestProvisioner(t)
		p.release = filename
		p.WithTemplate(TemplateOptions{BaseURL: "https://example.invalid"})
		if err := p.Init(context.Background()); err == nil {
			t.Fatalf("unsafe filename %q accepted", filename)
		}
	}
}

func TestTemplateRedirectDoesNotLeakCredentials(t *testing.T) {
	t.Setenv("TEMPLATE_HEADER", "Bearer secret")
	var destinationRequests int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationRequests++
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/archive", http.StatusFound)
	}))
	defer server.Close()
	p, _, _, _, _ := newTestProvisioner(t)
	p.WithTemplate(TemplateOptions{BaseURL: server.URL, Header: "Authorization", HeaderEnv: "TEMPLATE_HEADER"})
	_, err := p.ensureBaseArchive(context.Background(), "jail.tgz")
	if err == nil || destinationRequests != 0 || strings.Contains(err.Error(), "Bearer secret") {
		t.Fatalf("redirect result: requests=%d error=%v", destinationRequests, err)
	}
}

func TestStaticVNETRequiresDNSBeforeDownload(t *testing.T) {
	p, _, downloader, _, _ := newTestProvisioner(t)
	p.WithNetworkMode("vnet").WithVNETGateway("192.168.122.1")
	p.ip4Address = "192.168.122.100/24"
	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "no-dns"})
	if err == nil || !strings.Contains(err.Error(), "DNS") || downloader.count() != 0 {
		t.Fatalf("static VNET missing DNS: %v, downloads=%d", err, downloader.count())
	}

	p.WithDNSServers([]string{"not-an-IP"})
	if err := p.Init(context.Background()); err == nil {
		t.Fatal("invalid DNS address accepted")
	}
}

func TestSignedTemplateProvisioning(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		path string
		data string
	}{
		{"bin/sh", "template shell"},
		{"etc/resolv.conf", "nameserver 192.0.2.99\n"},
		{"usr/local/etc/organization.conf", "organization=true\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.path, Mode: 0755, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	key := templateSigningKey(t, nil)
	sig := templateSignature(t, key, archive.Bytes(), false, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write(sig)
		} else {
			_, _ = w.Write(archive.Bytes())
		}
	}))
	defer server.Close()
	for _, customDNS := range []bool{false, true} {
		t.Run(map[bool]string{false: "host DNS", true: "configured DNS"}[customDNS], func(t *testing.T) {
			p, runner, _, _, _ := newTestProvisioner(t)
			p.release = "organization.tgz"
			p.run = func(ctx context.Context, name string, args ...string) error {
				if name == "tar" {
					return defaultCommandRunner(ctx, name, args...)
				}
				if name == "jail" && len(args) > 0 && args[0] == "-c" {
					info, err := os.Stat(filepath.Join(p.getJailRootPath("wsd-signed"), "tmp"))
					if err != nil {
						t.Fatalf("/tmp must exist before jail startup: %v", err)
					}
					if !info.IsDir() || info.Mode().Perm() != 0777 || info.Mode()&os.ModeSticky == 0 {
						t.Fatalf("/tmp must be a sticky world-writable directory before startup: %v", info.Mode())
					}
				}
				return runner.run(ctx, name, args...)
			}
			p.WithTemplate(TemplateOptions{BaseURL: server.URL, VerifySignature: true, PublicKey: templatePublicKey(t, key, false)})
			if customDNS {
				p.WithDNSServers([]string{"192.0.2.53"})
			}
			handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "signed", WorkDir: "/workspace/repo"})
			if err != nil {
				t.Fatal(err)
			}
			root := p.getJailRootPath(handle.NativeID)
			config, err := os.ReadFile(filepath.Join(root, "usr/local/etc/organization.conf"))
			if err != nil || string(config) != "organization=true\n" {
				t.Fatalf("organization config: %q, %v", config, err)
			}
			dns, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
			if err != nil {
				t.Fatal(err)
			}
			want := []byte("nameserver 192.0.2.53\n")
			if !customDNS {
				want, err = os.ReadFile("/etc/resolv.conf")
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(dns, want) {
				t.Fatalf("DNS = %q, want %q", dns, want)
			}
			if !runner.calledWith("jail -c") {
				t.Fatal("verified template jail was not started")
			}
			if err := p.Destroy(context.Background(), handle); err != nil {
				t.Fatal(err)
			}
			if pathExists(root) {
				t.Fatal("template workspace root was not removed")
			}
			if _, err := p.ensureBaseArchive(context.Background(), p.release); err != nil {
				t.Fatalf("destroy removed or damaged shared template cache: %v", err)
			}
		})
	}
}

func TestTemplateGnuPGCompatibility(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("GnuPG not installed; runtime verification does not require it")
	}

	for _, algorithm := range []packet.PublicKeyAlgorithm{packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoRSA} {
		key := templateSigningKey(t, &packet.Config{Algorithm: algorithm, RSABits: 2048})
		pub := templatePublicKey(t, key, false)
		for _, armored := range []bool{false, true} {
			dir := t.TempDir()
			archive := filepath.Join(dir, "organization.tgz")
			payload := []byte("interoperability test")
			if err := os.WriteFile(archive, payload, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(archive+".sig", templateSignature(t, key, payload, armored, nil), 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(gpg, "--homedir", dir, "--batch", "--no-autostart",
				"--no-default-keyring", "--keyring", pub, "--verify", archive+".sig", archive)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("GnuPG rejected algorithm %d, armored=%t: %v\n%s", algorithm, armored, err, output)
			}
			p, _, _, _, _ := newTestProvisioner(t)
			p.template.PublicKey = pub
			if err := p.verifyTemplate(archive, archive+".sig"); err != nil {
				t.Fatalf("native verifier rejected GnuPG-compatible signature: %v", err)
			}
		}
	}
}

func TestTemplateDNSCannotFollowSymlinkOutsideRoot(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("creating symlinks requires extra privileges on Windows")
	}
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "host-resolv.conf")
	if err := os.WriteFile(external, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	for _, escalator := range []string{"", "mdo"} {
		t.Run("escalator="+escalator, func(t *testing.T) {
			p, runner, _, _, _ := newTestProvisioner(t)
			p.WithEscalationCommand(escalator).WithDNSServers([]string{"192.0.2.53"})
			if err := p.copyHostResolvConf(context.Background(), root); err == nil {
				t.Fatal("DNS write escaped the template root")
			}
			if runner.calledWith("chroot") {
				t.Fatal("privileged write attempted for an escaping symlink")
			}
		})
	}
	data, err := os.ReadFile(external)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("host file changed: %q, %v", data, err)
	}
}
