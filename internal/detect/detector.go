package detect

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/davidcrumpton/mcwsd/internal"
)

// dockerHubLibrary is the canonical registry prefix for official Docker Hub
// images. Docker implicitly resolves bare names like "golang" to this prefix,
// but Podman only does so for names present in its short-name alias list
// (e.g. "ubuntu") and otherwise fails with "did not resolve to an alias and no
// unqualified-search registries are defined". Emitting fully-qualified
// references keeps the same base image resolvable by both runtimes.
const dockerHubLibrary = "docker.io/library/"

// Detector implements EnvironmentDetector with priority-based detection.
type Detector struct{}

// NewDetector creates a new environment detector.
func NewDetector() *Detector {
	return &Detector{}
}

// Detect scans repoPath and returns an EnvironmentProfile based on priority order.
func (d *Detector) Detect(repoPath string) (*internal.EnvironmentProfile, error) {
	if entries, err := os.ReadDir(repoPath); err == nil && len(entries) == 0 {
		return d.detectBlank(repoPath)
	}

	// Priority 1: .devcontainer/devcontainer.json
	if profile, err := d.detectDevcontainer(repoPath); err == nil && profile != nil {
		return profile, nil
	}

	// Priority 2: go.mod
	if profile, err := d.detectGo(repoPath); err == nil && profile != nil {
		return profile, nil
	}

	// Priority 3: Cargo.toml + optional rust-toolchain.toml
	if profile, err := d.detectRust(repoPath); err == nil && profile != nil {
		return profile, nil
	}

	// Priority 4: pyproject.toml
	if profile, err := d.detectPython(repoPath); err == nil && profile != nil {
		return profile, nil
	}

	// Priority 5: requirements.txt (no pyproject.toml)
	if profile, err := d.detectPipRequirements(repoPath); err == nil && profile != nil {
		return profile, nil
	}

	// Priority 6: package.json
	if profile, err := d.detectNode(repoPath); err == nil && profile != nil {
		return profile, nil
	}

	// Priority 7: none matched
	if profile, err := d.detectUnknown(repoPath); err == nil && profile != nil {
		return profile, nil
	}
	return nil, nil
}

// DevcontainerConfig represents devcontainer.json structure.
type DevcontainerConfig struct {
	Image string `json:"image"`
	Build struct {
		Dockerfile string `json:"dockerfile"`
		Context    string `json:"context"`
	} `json:"build"`
	PostCreateCommand string `json:"postCreateCommand"`
}

func (d *Detector) detectDevcontainer(repoPath string) (*internal.EnvironmentProfile, error) {
	dcPath := filepath.Join(repoPath, ".devcontainer", "devcontainer.json")
	data, err := os.ReadFile(dcPath)
	if err != nil {
		return nil, err // file doesn't exist or unreadable
	}

	var config DevcontainerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid devcontainer.json: %w", err)
	}

	profile := &internal.EnvironmentProfile{
		Language:          "devcontainer",
		Version:           "latest",
		PackageManager:    "none",
		InstallCommand:    "",
		BaseImage:         config.Image,
		PostCreateCommand: config.PostCreateCommand,
	}

	// If image is specified, use it directly
	if config.Image != "" {
		return profile, nil
	}

	// If build.dockerfile is specified, we'd need to build - for now skip and fall through
	if config.Build.Dockerfile != "" {
		// Has Dockerfile build - fall through to other detectors but keep postCreateCommand
		// For simplicity, return error to trigger fallback
		return nil, nil
	}

	// devcontainer exists but only configures editor extensions - fall through
	// Return nil to indicate we should continue to next detector
	return nil, nil
}

func (d *Detector) detectGo(repoPath string) (*internal.EnvironmentProfile, error) {
	// Extract Go version from "go X.Y" directive
	version := "1.21" // default

	goModPath := filepath.Join(repoPath, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err == nil {
		re := regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
		if matches := re.FindStringSubmatch(string(data)); len(matches) > 1 {
			version = matches[1]
		}
	} else if !hasGoFiles(repoPath) {
		// No go.mod and no .go source files anywhere: not a Go repository.
		return nil, err
	}

	return &internal.EnvironmentProfile{
		Language:       "go",
		Version:        version,
		PackageManager: "go mod",
		InstallCommand: "go mod download",
		BaseImage:      fmt.Sprintf("%sgolang:%s", dockerHubLibrary, version),
	}, nil
}

// hasGoFiles reports whether repoPath contains any .go source file, so a Go
// repository without a go.mod (single-file programs, pre-module layouts) is
// still recognized. .git and vendor subtrees are skipped.
func hasGoFiles(repoPath string) bool {
	found := false
	_ = filepath.WalkDir(repoPath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func (d *Detector) detectRust(repoPath string) (*internal.EnvironmentProfile, error) {
	cargoPath := filepath.Join(repoPath, "Cargo.toml")
	if _, err := os.Stat(cargoPath); err != nil {
		return nil, err
	}

	version := "stable"
	toolchainPath := filepath.Join(repoPath, "rust-toolchain.toml")
	if data, err := os.ReadFile(toolchainPath); err == nil {
		// Parse rust-toolchain.toml for channel
		re := regexp.MustCompile(`channel\s*=\s*"([^"]+)"`)
		if matches := re.FindStringSubmatch(string(data)); len(matches) > 1 {
			version = matches[1]
		}
	}

	return &internal.EnvironmentProfile{
		Language:       "rust",
		Version:        version,
		PackageManager: "cargo",
		InstallCommand: fmt.Sprintf("rustup install %s && rustup default %s && cargo build", version, version),
		BaseImage:      dockerHubLibrary + "rust:latest",
	}, nil
}

func (d *Detector) detectPython(repoPath string) (*internal.EnvironmentProfile, error) {
	pyprojectPath := filepath.Join(repoPath, "pyproject.toml")
	if _, err := os.Stat(pyprojectPath); err != nil {
		return nil, err
	}

	// Check for poetry.lock
	poetryLockPath := filepath.Join(repoPath, "poetry.lock")
	_, hasPoetryLock := os.Stat(poetryLockPath)

	// Check for .python-version
	pythonVersion := "3.11" // default
	if verData, err := os.ReadFile(filepath.Join(repoPath, ".python-version")); err == nil {
		pythonVersion = strings.TrimSpace(string(verData))
	}

	if hasPoetryLock == nil {
		// Use Poetry
		return &internal.EnvironmentProfile{
			Language:       "python",
			Version:        pythonVersion,
			PackageManager: "poetry",
			InstallCommand: "poetry install",
			BaseImage:      fmt.Sprintf("%spython:%s", dockerHubLibrary, pythonVersion),
		}, nil
	}

	// No poetry.lock - use pip install -e .
	return &internal.EnvironmentProfile{
		Language:       "python",
		Version:        pythonVersion,
		PackageManager: "pip",
		InstallCommand: "python -m venv .venv && .venv/bin/pip install -e .",
		BaseImage:      fmt.Sprintf("%spython:%s", dockerHubLibrary, pythonVersion),
	}, nil
}

func (d *Detector) detectPipRequirements(repoPath string) (*internal.EnvironmentProfile, error) {
	reqPath := filepath.Join(repoPath, "requirements.txt")
	if _, err := os.Stat(reqPath); err != nil {
		return nil, err
	}

	// Also check that pyproject.toml doesn't exist (caller's responsibility per spec)
	pyprojectPath := filepath.Join(repoPath, "pyproject.toml")
	if _, err := os.Stat(pyprojectPath); err == nil {
		return nil, fmt.Errorf("pyproject.toml exists, should use detectPython instead")
	}

	pythonVersion := "3.11"
	if verData, err := os.ReadFile(filepath.Join(repoPath, ".python-version")); err == nil {
		pythonVersion = strings.TrimSpace(string(verData))
	}

	return &internal.EnvironmentProfile{
		Language:       "python",
		Version:        pythonVersion,
		PackageManager: "pip",
		InstallCommand: "python -m venv .venv && .venv/bin/pip install -r requirements.txt",
		BaseImage:      fmt.Sprintf("%spython:%s", dockerHubLibrary, pythonVersion),
	}, nil
}

func (d *Detector) detectNode(repoPath string) (*internal.EnvironmentProfile, error) {
	pkgJSONPath := filepath.Join(repoPath, "package.json")
	data, err := os.ReadFile(pkgJSONPath)
	if err != nil {
		return nil, err
	}

	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("invalid package.json: %w", err)
	}

	// Determine package manager
	packageManager := "npm"
	installCommand := "npm ci"

	// Check packageManager field first (Corepack-style)
	if pkg.PackageManager != "" {
		// e.g., "pnpm@8.6.0" or "yarn@3.2.0"
		parts := strings.Split(pkg.PackageManager, "@")
		if len(parts) >= 1 {
			packageManager = parts[0]
		}
		switch packageManager {
		case "pnpm":
			installCommand = "pnpm install"
		case "yarn":
			installCommand = "yarn install"
		case "npm":
			installCommand = "npm ci"
		}
	} else {
		// Check for lock files
		if _, err := os.Stat(filepath.Join(repoPath, "pnpm-lock.yaml")); err == nil {
			packageManager = "pnpm"
			installCommand = "pnpm install"
		} else if _, err := os.Stat(filepath.Join(repoPath, "yarn.lock")); err == nil {
			packageManager = "yarn"
			installCommand = "yarn install"
		} else if _, err := os.Stat(filepath.Join(repoPath, "package-lock.json")); err == nil {
			packageManager = "npm"
			installCommand = "npm ci"
		}
	}

	return &internal.EnvironmentProfile{
		Language:       "node",
		Version:        "latest",
		PackageManager: packageManager,
		InstallCommand: installCommand,
		BaseImage:      dockerHubLibrary + "node:latest",
	}, nil
}

// genericToolchainImage is the fallback base image for repositories whose
// language and package manager could not be detected. It is deliberately
// broad (git, curl, build tools and common development libraries) so an agent
// can inspect the repo and install whatever the project actually needs at
// runtime instead of the daemon guessing up front. It is only meaningful to
// container runtimes; the backend blanks it once the runtime is known to be a
// jail/zone/vm/process runtime (which ignore BaseImage).
const genericToolchainImage = dockerHubLibrary + "buildpack-deps:stable"

func (d *Detector) detectUnknown(repoPath string) (*internal.EnvironmentProfile, error) {
	// Language stays "unknown" and InstallCommand stays empty on purpose:
	// there is nothing to auto-install. The agent is expected to inspect the
	// repo and set the environment up itself (see the mcwsd tool contract).
	//
	// PackageManager "none" means only that no *project* package manager was
	// recognized. It does NOT mean the runtime has no package manager; the
	// backend fills in SystemPackageManager/SystemInstallCommand once the
	// runtime OS is known. Say so explicitly, because agents otherwise read
	// "none" as "I cannot install anything".
	return &internal.EnvironmentProfile{
		Language:       "unknown",
		Version:        "latest",
		PackageManager: "none",
		InstallCommand: "",
		BaseImage:      genericToolchainImage,
		Notes: "No project language or package manager was detected from the repo files. " +
			"This is not an error. package_manager \"none\" refers to the project's own dependencies only; " +
			"the runtime OS still has a system package manager (see system_package_manager/system_install_command). " +
			"Inspect the repo (README, Makefile, build files, source) to determine how to build it, and install any " +
			"missing system tools with the system install command.",
	}, nil
}

func (d *Detector) detectBlank(repoPath string) (*internal.EnvironmentProfile, error) {
	return &internal.EnvironmentProfile{
		Language:       "blank",
		Version:        "latest",
		PackageManager: "none",
		BaseImage:      genericToolchainImage,
		Notes: "This is a blank workspace with no repository files. " +
			"Create or write the project files first, then inspect them before installing dependencies.",
	}, nil
}

// ApplyOverrides patches a detected EnvironmentProfile with caller-supplied
// values. It is the explicit escape hatch for repositories whose environment
// cannot be (or should not be) auto-detected: the caller pins whatever it
// already knows. Only non-empty values are applied; unknown keys are ignored.
//
// Supported keys mirror the EnvironmentProfile JSON tags:
//
//	language, version, package_manager, install_command, base_image,
//	post_create_command, system_package_manager, system_install_command,
//	notes
//
// If profile is nil a new one is allocated, so an override can fully define
// the environment without any detection at all.
func ApplyOverrides(profile *internal.EnvironmentProfile, overrides map[string]string) *internal.EnvironmentProfile {
	if len(overrides) == 0 {
		return profile
	}
	if profile == nil {
		profile = &internal.EnvironmentProfile{}
	}
	for key, value := range overrides {
		if value == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "language":
			profile.Language = value
		case "version":
			profile.Version = value
		case "package_manager":
			profile.PackageManager = value
		case "install_command":
			profile.InstallCommand = value
		case "base_image":
			profile.BaseImage = value
		case "post_create_command":
			profile.PostCreateCommand = value
		case "system_package_manager":
			profile.SystemPackageManager = value
		case "system_install_command":
			profile.SystemInstallCommand = value
		case "notes":
			profile.Notes = value
		}
	}
	return profile
}
