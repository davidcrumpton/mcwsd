package detect

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
)

// writeFile is a test helper that creates a file with given content under dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("writeFile mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("writeFile write %s: %v", name, err)
	}
}

func newDetector() *Detector { return NewDetector() }

func TestDetect_BlankWorkspace(t *testing.T) {
	dir := t.TempDir()

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p == nil {
		t.Fatal("Detect returned nil profile")
	}
	if p.Language != "blank" {
		t.Errorf("Language: want %q, got %q", "blank", p.Language)
	}
	if p.PackageManager != "none" {
		t.Errorf("PackageManager: want %q, got %q", "none", p.PackageManager)
	}
	if p.InstallCommand != "" {
		t.Errorf("InstallCommand: want empty, got %q", p.InstallCommand)
	}
}

// ---------------------------------------------------------------------------
// Go
// ---------------------------------------------------------------------------

func TestDetect_GoMod(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.22\n")

	d := newDetector()
	p, err := d.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if p.Language != "go" {
		t.Errorf("Language: want %q, got %q", "go", p.Language)
	}
	if p.Version != "1.22" {
		t.Errorf("Version: want %q, got %q", "1.22", p.Version)
	}
	if p.PackageManager != "go mod" {
		t.Errorf("PackageManager: want %q, got %q", "go mod", p.PackageManager)
	}
	if p.InstallCommand != "go mod download" {
		t.Errorf("InstallCommand: want %q, got %q", "go mod download", p.InstallCommand)
	}
	if p.BaseImage != "docker.io/library/golang:1.22" {
		t.Errorf("BaseImage: want %q, got %q", "docker.io/library/golang:1.22", p.BaseImage)
	}
}

func TestDetect_GoMod_DefaultVersion(t *testing.T) {
	dir := t.TempDir()
	// go.mod with no 'go' directive.
	writeFile(t, dir, "go.mod", "module example.com/app\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Version != "1.21" {
		t.Errorf("Version default: want %q, got %q", "1.21", p.Version)
	}
}

// ---------------------------------------------------------------------------
// Rust
// ---------------------------------------------------------------------------

func TestDetect_Rust_Stable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Cargo.toml", "[package]\nname = \"myapp\"\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "rust" {
		t.Errorf("Language: want %q, got %q", "rust", p.Language)
	}
	if p.Version != "stable" {
		t.Errorf("Version: want %q, got %q", "stable", p.Version)
	}
	if p.PackageManager != "cargo" {
		t.Errorf("PackageManager: want %q, got %q", "cargo", p.PackageManager)
	}
}

func TestDetect_Rust_WithToolchain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Cargo.toml", "[package]\nname = \"myapp\"\n")
	writeFile(t, dir, "rust-toolchain.toml", "[toolchain]\nchannel = \"nightly\"\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Version != "nightly" {
		t.Errorf("Version with toolchain: want %q, got %q", "nightly", p.Version)
	}
}

// ---------------------------------------------------------------------------
// Python — pyproject.toml
// ---------------------------------------------------------------------------

func TestDetect_Python_PoetryLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[tool.poetry]\nname = \"app\"\n")
	writeFile(t, dir, "poetry.lock", "# lock file\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "python" {
		t.Errorf("Language: want %q, got %q", "python", p.Language)
	}
	if p.PackageManager != "poetry" {
		t.Errorf("PackageManager: want %q, got %q", "poetry", p.PackageManager)
	}
	if p.InstallCommand != "poetry install" {
		t.Errorf("InstallCommand: want %q, got %q", "poetry install", p.InstallCommand)
	}
}

func TestDetect_Python_PipEditable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[project]\nname = \"app\"\n")
	// No poetry.lock → pip install -e .

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.PackageManager != "pip" {
		t.Errorf("PackageManager: want %q, got %q", "pip", p.PackageManager)
	}
	wantCmd := "python -m venv .venv && .venv/bin/pip install -e ."
	if p.InstallCommand != wantCmd {
		t.Errorf("InstallCommand: want %q, got %q", wantCmd, p.InstallCommand)
	}
}

func TestDetect_Python_CustomVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[project]\nname = \"app\"\n")
	writeFile(t, dir, ".python-version", "3.12\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Version != "3.12" {
		t.Errorf("Version: want %q, got %q", "3.12", p.Version)
	}
}

// ---------------------------------------------------------------------------
// Python — requirements.txt
// ---------------------------------------------------------------------------

func TestDetect_PipRequirements(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "requirements.txt", "requests\nflask\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "python" {
		t.Errorf("Language: want %q, got %q", "python", p.Language)
	}
	if p.PackageManager != "pip" {
		t.Errorf("PackageManager: want %q, got %q", "pip", p.PackageManager)
	}
	wantCmd := "python -m venv .venv && .venv/bin/pip install -r requirements.txt"
	if p.InstallCommand != wantCmd {
		t.Errorf("InstallCommand: want %q, got %q", wantCmd, p.InstallCommand)
	}
}

func TestDetect_PipRequirements_PyprojectTakenPriority(t *testing.T) {
	dir := t.TempDir()
	// When BOTH pyproject.toml and requirements.txt exist,
	// pyproject.toml (priority 4) must win over requirements.txt (priority 5).
	writeFile(t, dir, "pyproject.toml", "[project]\nname = \"app\"\n")
	writeFile(t, dir, "requirements.txt", "flask\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	// Expect pip install -e . because no poetry.lock.
	wantCmd := "python -m venv .venv && .venv/bin/pip install -e ."
	if p.InstallCommand != wantCmd {
		t.Errorf("InstallCommand: want %q, got %q", wantCmd, p.InstallCommand)
	}
}

// ---------------------------------------------------------------------------
// Node
// ---------------------------------------------------------------------------

func TestDetect_Node_NpmDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"app"}`)

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "node" {
		t.Errorf("Language: want %q, got %q", "node", p.Language)
	}
	if p.PackageManager != "npm" {
		t.Errorf("PackageManager: want %q, got %q", "npm", p.PackageManager)
	}
}

func TestDetect_Node_PnpmLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"app"}`)
	writeFile(t, dir, "pnpm-lock.yaml", "# pnpm lock\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.PackageManager != "pnpm" {
		t.Errorf("PackageManager: want %q, got %q", "pnpm", p.PackageManager)
	}
	if p.InstallCommand != "pnpm install" {
		t.Errorf("InstallCommand: want %q, got %q", "pnpm install", p.InstallCommand)
	}
}

func TestDetect_Node_YarnLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"app"}`)
	writeFile(t, dir, "yarn.lock", "# yarn lock\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.PackageManager != "yarn" {
		t.Errorf("PackageManager: want %q, got %q", "yarn", p.PackageManager)
	}
}

func TestDetect_Node_PackageManagerField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"app","packageManager":"pnpm@8.6.0"}`)

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.PackageManager != "pnpm" {
		t.Errorf("PackageManager from field: want %q, got %q", "pnpm", p.PackageManager)
	}
}

func TestDetect_Unknown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# app\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "unknown" {
		t.Errorf("Language: want %q, got %q", "unknown", p.Language)
	}
	if p.PackageManager != "none" {
		t.Errorf("PackageManager: want %q, got %q", "none", p.PackageManager)
	}
	if p.InstallCommand != "" {
		t.Errorf("InstallCommand: want %q, got %q", "", p.InstallCommand)
	}
	wantImage := genericToolchainImage
	if goruntime.GOOS == "freebsd" {
		// On FreeBSD the jail provisioner supplies the base release; the
		// detector must not hand it a Linux container image reference.
		wantImage = ""
	}
	if p.BaseImage != wantImage {
		t.Errorf("BaseImage: want %q, got %q", wantImage, p.BaseImage)
	}
	if p.Notes == "" {
		t.Error("Notes: want non-empty guidance for the unknown profile, got empty")
	}
}

// ---------------------------------------------------------------------------
// Devcontainer
// ---------------------------------------------------------------------------

func TestDetect_Devcontainer_WithImage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".devcontainer/devcontainer.json",
		`{"image":"mcr.microsoft.com/devcontainers/go:1.22","postCreateCommand":"go mod download"}`)

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "devcontainer" {
		t.Errorf("Language: want %q, got %q", "devcontainer", p.Language)
	}
	if p.BaseImage != "mcr.microsoft.com/devcontainers/go:1.22" {
		t.Errorf("BaseImage: want %q, got %q", "mcr.microsoft.com/devcontainers/go:1.22", p.BaseImage)
	}
	if p.PostCreateCommand != "go mod download" {
		t.Errorf("PostCreateCommand: want %q, got %q", "go mod download", p.PostCreateCommand)
	}
}

func TestDetect_Priority_DevcontainerBeforeGoMod(t *testing.T) {
	dir := t.TempDir()
	// Both present — devcontainer must win.
	writeFile(t, dir, ".devcontainer/devcontainer.json",
		`{"image":"custom:latest"}`)
	writeFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.22\n")

	p, err := newDetector().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Language != "devcontainer" {
		t.Errorf("Priority: want devcontainer, got %q", p.Language)
	}
}

// ---------------------------------------------------------------------------
// EnvOverrides
// ---------------------------------------------------------------------------

func TestApplyOverrides_PinsEnvironment(t *testing.T) {
	base := &internal.EnvironmentProfile{
		Language:       "unknown",
		Version:        "latest",
		PackageManager: "none",
		BaseImage:      genericToolchainImage,
	}

	p := ApplyOverrides(base, map[string]string{
		"language":        "node",
		"version":         "20",
		"package_manager": "npm",
		"install_command": "npm ci",
		"base_image":      "node:20",
	})

	if p != base {
		t.Fatal("ApplyOverrides should patch the profile in place")
	}
	if p.Language != "node" || p.Version != "20" || p.PackageManager != "npm" ||
		p.InstallCommand != "npm ci" || p.BaseImage != "node:20" {
		t.Errorf("overrides not applied: %+v", p)
	}
}

func TestApplyOverrides_IgnoresEmptyAndUnknownKeys(t *testing.T) {
	base := &internal.EnvironmentProfile{Language: "go", Version: "1.22"}
	p := ApplyOverrides(base, map[string]string{
		"language": "",
		"bogus":    "x",
	})
	if p.Language != "go" || p.Version != "1.22" {
		t.Errorf("unexpected mutation: %+v", p)
	}
}

func TestApplyOverrides_NilProfile(t *testing.T) {
	p := ApplyOverrides(nil, map[string]string{"base_image": "alpine:latest"})
	if p == nil || p.BaseImage != "alpine:latest" {
		t.Fatalf("nil profile override: %+v", p)
	}
}

func TestApplyOverrides_NoOverridesReturnsSameProfile(t *testing.T) {
	base := &internal.EnvironmentProfile{Language: "go"}
	if got := ApplyOverrides(base, nil); got != base {
		t.Fatal("nil overrides should return the same profile")
	}
}

func TestDetect_Devcontainer_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".devcontainer/devcontainer.json", `{not valid}`)
	// Also provide go.mod so we fall through to a valid detector.
	writeFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.21\n")

	p, err := newDetector().Detect(dir)
	// Bad devcontainer JSON → should fall through to go.mod.
	if err != nil {
		t.Fatalf("Detect with invalid devcontainer.json: %v", err)
	}
	if p.Language != "go" {
		t.Errorf("expected fallback to go, got %q", p.Language)
	}
}
