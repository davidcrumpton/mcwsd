# Packaging and Releases

`mcwsd` ships through six channels, all produced from one tag push by
[GoReleaser](https://goreleaser.com) in `.github/workflows/release.yml`:

| Channel | Artifact | Installed by |
| --- | --- | --- |
| GitHub Releases | `mcwsd_<ver>_<os>_<arch>.tar.gz` / `.zip` | `curl`/manual, `go install` |
| Debian / Ubuntu | `mcwsd_<ver>_linux_<arch>.deb` | `apt install ./file.deb` |
| Fedora / RHEL / openSUSE | `mcwsd-<ver>-1.<arch>.rpm` | `dnf install ./file.rpm` |
| Alpine | `mcwsd_<ver>_linux_<arch>.apk` | `apk add --allow-untrusted ./file.apk` |
| macOS / Linuxbrew | formula in `davidcrumpton/homebrew-tap` | `brew install davidcrumpton/tap/mcwsd` |
| Windows | manifest in `davidcrumpton/scoop-bucket` | `scoop install mcwsd` |
| Arch Linux | `packaging/aur/PKGBUILD` (published to AUR by hand) | `yay -S mcwsd` |
| FreeBSD | `packaging/freebsd/` port skeleton | `pkg install mcwsd` (once in the tree) |
| Container | `ghcr.io/davidcrumpton/mcwsd` | `docker pull` |

Everything except the last three is automatic.

## What a release contains

Each release publishes, for `linux`, `darwin`, `freebsd` and `windows` on
`amd64` and `arm64`:

* the `mcwsd` binary plus `plugins/*.js` (the daemon has **no default plugin
  search path** — `--plugin` is mandatory, so the plugins travel with it)
* `LICENSE`, `README.md`, `PLUGINS.md`, `ARCHITECTURE.md`
* a systemd unit and an env-file template under `packaging/systemd/`

and additionally:

* `checksums.txt` — SHA-256 of every artifact
* `*.spdx.json` — SPDX SBOM per archive
* `checksums.txt.sigstore.json` — keyless cosign signature (GitHub OIDC)
* SLSA build provenance attestations for all archives and packages

## Package layout (deb / rpm / apk)

| Path | Contents |
| --- | --- |
| `/usr/bin/mcwsd` | binary |
| `/usr/share/mcwsd/plugins/*.js` | all nine runtime plugins |
| `/usr/lib/systemd/system/mcwsd.service` | unit (deb, rpm) |
| `/usr/lib/sysusers.d/mcwsd.conf` | creates `_wsd` (deb, rpm) |
| `/usr/lib/tmpfiles.d/mcwsd.conf` | `/var/lib/mcwsd`, `/var/log/mcwsd` (deb, rpm) |
| `/etc/mcwsd/mcwsd.env` | secrets/config template (`config\|noreplace`) |
| `/etc/init.d/mcwsd` + `/etc/conf.d/mcwsd` | OpenRC service (apk only) |
| `/usr/share/doc/mcwsd/` | LICENSE, README, PLUGINS |

Design notes:

* The daemon is **not enabled or started** by the package: it refuses to run
  without `MCWSD_TOKEN`, so the postinstall prints the two commands to run once
  the token is in `/etc/mcwsd/mcwsd.env`.
* No dependency on a container runtime — the runtime is selected per instance
  with `--plugin` (docker, podman, kubernetes, ...). Postinstall adds `_wsd` to
  the `docker`/`podman` group **if those groups exist**, since the daemon shells
  out to those CLIs. That is root-equivalent access; see `packaging/scripts/`.
* Removal keeps `/var/lib/mcwsd`, `/var/log/mcwsd`, `/etc/mcwsd` and the `_wsd`
  user: they hold workspaces, audit history and credentials.

## One-time setup

1. ✅ **Homebrew tap repo** — [davidcrumpton/homebrew-tap](https://github.com/davidcrumpton/homebrew-tap)
   exists (public, `main`, `Formula/` seeded with a `.gitkeep`).
2. ✅ **Scoop bucket repo** — [davidcrumpton/scoop-bucket](https://github.com/davidcrumpton/scoop-bucket)
   exists (public, `main`, `bucket/` seeded with a `.gitkeep`).
3. ✅ **`TAP_GITHUB_TOKEN` secret** — set on `davidcrumpton/mcwsd`. It is a
   classic PAT with `repo` + `workflow` scopes, which also grants contents
   write on both tap repositories. The workflow's built-in `GITHUB_TOKEN`
   cannot push to a different repository, which is why a PAT is needed.
   Rotate it at <https://github.com/settings/tokens> and re-run
   <https://github.com/davidcrumpton/mcwsd/settings/secrets/actions> if it
   expires; a release fails at the `homebrew formula` step if it does.
4. ⬜ *(Optional)* branch protection on `main`, requiring the `CI` workflow.

The seeded `.gitkeep` files only exist because git cannot track an empty
directory; GoReleaser writes real files next to them (`Formula/mcwsd.rb`,
`bucket/mcwsd.json`).

## Cutting a release

```sh
# 1. The tag is the version: goreleaser stamps main.Version from it.
#    (The source default is "0.5.006"; tags use normal semver: v0.5.6.)
git tag -a v0.5.6 -m "mcwsd 0.5.6"

# 2. Push to the GitHub remote (origin is GitLab in this checkout).
git push github v0.5.6
```

That triggers `Release` → GoReleaser publishes the release, updates the tap and
the Scoop bucket, signs the checksums, attests the artifacts, and the
`container` job pushes a multi-arch image to `ghcr.io`.

Dry run locally (no publishing, no credentials needed):

```sh
make release-check        # goreleaser check + snapshot release
make snapshot             # just the build matrix
```

## Verifying a download

```sh
# Checksums are signed with cosign in keyless mode (GitHub OIDC identity).
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/davidcrumpton/mcwsd/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

sha256sum -c checksums.txt --ignore-missing

# SLSA provenance
gh attestation verify mcwsd_0.5.6_linux_amd64.deb --repo davidcrumpton/mcwsd
```

## Install commands

```sh
# Homebrew (macOS, Linuxbrew)
brew install davidcrumpton/tap/mcwsd
mcwsd daemon --plugin "$(brew --prefix)/opt/mcwsd/share/mcwsd/plugins/docker.js" \
  --workspace-auth-mode=scoped --port 9080

# Debian / Ubuntu
sudo apt install ./mcwsd_0.5.6_linux_amd64.deb
sudo -e /etc/mcwsd/mcwsd.env     # set MCWSD_TOKEN (from `mcwsd token`)
sudo systemctl enable --now mcwsd

# Fedora / RHEL
sudo dnf install ./mcwsd-0.5.6-1.x86_64.rpm

# Alpine
sudo apk add --allow-untrusted ./mcwsd_0.5.6_linux_amd64.apk
rc-update add mcwsd default && rc-service mcwsd start

# Windows
scoop install mcwsd

# From source
go install github.com/davidcrumpton/mcwsd/cmd/mcwsd@v0.5.6
```

## Arch Linux (AUR)

`packaging/aur/PKGBUILD` builds from the tagged source tarball.

```sh
./packaging/aur/update-aur.sh 0.5.6      # refresh pkgver + sha256sums + .SRCINFO
git clone ssh://aur@aur.archlinux.org/mcwsd.git
cp packaging/aur/PKGBUILD packaging/aur/.SRCINFO mcwsd.git/
cd mcwsd.git && git add -A && git commit -m "mcwsd 0.5.6" && git push
```

The AUR is updated by hand rather than by CI because it needs a separate AUR SSH
key; wire `goreleaser aurs` (or a small `ssh` step) in if you would rather
automate it.

## FreeBSD

`packaging/freebsd/` is a port skeleton (`sysutils/mcwsd`) using
`USES=go:modules` with an rc.d script and the `_wsd` user. Copy it into a ports
tree, then:

```sh
make makesum          # generates distinfo for the Go module deps
make stage && make check-plist && portlint -AC
```

## Container image

The `container` job builds the existing `Dockerfile` for `linux/amd64` and
`linux/arm64`, stamps `MCWSD_VERSION` from the tag, and pushes
`ghcr.io/davidcrumpton/mcwsd:<version>`, `:<major>.<minor>` and `:latest`
(skipped for pre-release tags). See `OCI-CONTAINER-SETUP.md` for runtime usage.

## Troubleshooting

* **`Refusing to load formula ... from untrusted tap`** — Homebrew 7 requires
  third-party taps to be trusted before it will load anything from them. Either
  trust the tap or install with an explicit override:

  ```sh
  brew trust davidcrumpton/tap      # once
  brew install davidcrumpton/tap/mcwsd
  ```

  The formula itself is unchanged; this is a Homebrew-side policy, and the same
  prompt appears for any tap outside `Homebrew/*`.
* **`mcwsd version` shows the old number** — the binary was built without
  `-X main.Version=...`; `make build` and goreleaser both do this, plain
  `go build` does not.
* **Release fails on the brew step** — `TAP_GITHUB_TOKEN` is missing or lacks
  write access to the tap/bucket repos.
* **`goreleaser check` warns about `brews`** — GoReleaser v2 prefers
  `homebrew_casks`; formulae are still supported and are used here because they
  work on Linuxbrew too.
* **Package installs but the service will not start** — `MCWSD_TOKEN` is unset
  in `/etc/mcwsd/mcwsd.env`; `journalctl -u mcwsd` shows the error.
