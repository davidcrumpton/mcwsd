# mcwsd — build, test and release helpers.
#
# Everything a release needs is driven by GoReleaser (.goreleaser.yaml); this
# Makefile is the local front end for it.

SHELL := /bin/sh
BINARY := mcwsd
MAIN := ./cmd/mcwsd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
PREFIX ?= /usr/local
PLUGIN_DIR := $(PREFIX)/share/mcwsd/plugins

.DEFAULT_GOAL := build

.PHONY: help
help:
	@echo "Targets:"
	@echo "  build            build ./$(BINARY) with the current git version"
	@echo "  test             go test ./..."
	@echo "  vet              go vet ./..."
	@echo "  fmt              gofmt -w the tree"
	@echo "  lint             golangci-lint (if installed)"
	@echo "  check            vet + gofmt check + test (what CI runs)"
	@echo "  snapshot         goreleaser snapshot build into dist/ (no publish)"
	@echo "  release-check    validate .goreleaser.yaml and build a snapshot"
	@echo "  install          install binary + plugins under PREFIX ($(PREFIX))"
	@echo "  docker-build     build the container image locally"
	@echo "  clean            remove build output"

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) $(MAIN)

.PHONY: test
test:
	go test ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	gofmt -w cmd internal pkg tools

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed; see https://golangci-lint.run/usage/install/"; exit 0; }
	golangci-lint run --timeout=5m ./...

.PHONY: check
check: vet
	@unformatted="$$(gofmt -l cmd internal pkg tools)"; \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	go test ./...

.PHONY: snapshot
snapshot:
	goreleaser build --snapshot --clean

.PHONY: release-check
release-check:
	# No cosign/syft needed: SBOMs and signatures are exercised in CI and on
	# the real release, not locally.
	goreleaser release --snapshot --clean --skip=publish,sbom,sign

.PHONY: install
install: build
	install -d $(DESTDIR)$(PREFIX)/bin $(DESTDIR)$(PLUGIN_DIR)
	install -m 0755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -m 0644 plugins/*.js $(DESTDIR)$(PLUGIN_DIR)/
	@echo "Installed. Start with:"
	@echo "  $(PREFIX)/bin/mcwsd daemon --plugin $(PLUGIN_DIR)/docker.js --workspace-auth-mode=scoped"

.PHONY: docker-build
docker-build:
	docker build --build-arg MCWSD_VERSION=$(VERSION) -t mcwsd:$(VERSION) .

.PHONY: clean
clean:
	rm -rf $(BINARY) dist
