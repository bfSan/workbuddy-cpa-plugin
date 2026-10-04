.PHONY: build test test-panel lint clean release

GO ?= go
NODE ?= node
VERSION ?= $(shell cat VERSION 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

# Default target: build the plugin for the current platform.
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o workbuddy.so .

# Run all tests with race detector.
test:
	$(GO) test -race -count=1 ./...
	$(NODE) --test panel.test.js

# Browser check of the credit-consistency warning, against the LIVE deployment.
# Separate from `test` because it needs a running CPA and a management key, and
# because the poisoned half of it intercepts API responses in the browser.
# PW_DIR must point at a node_modules tree holding playwright; the script exits 77
# when it is absent.
test-panel:
	$(NODE) scripts/credit-warn-check.js

# Lint everything (requires gofmt + go vet; staticcheck/gocritic/unparam optional).
lint:
	@test -z "$$($(GO)fmt -l .)" || ($(GO)fmt -l . && exit 1)
	$(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; fi
	@if command -v gocritic >/dev/null 2>&1; then gocritic check ./...; fi
	@if command -v unparam >/dev/null 2>&1; then unparam ./...; fi

# Clean build artifacts.
clean:
	rm -f workbuddy.so workbuddy.h
	rm -rf bin/ dist/ coverage.out coverage.html

# Cross-compile for all supported platforms (requires cross C toolchains).
# On a Linux host without osxcross, only linux/* targets will succeed.
release: clean
	@mkdir -p dist
	@set -e; for target in linux/amd64 linux/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  out=dist/workbuddy_$(VERSION)_$${os}_$${arch}; \
	  mkdir -p $$out; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $$out/workbuddy.so .; \
	  test -f $$out/workbuddy.so; \
	  cp README.md README_CN.md LICENSE $$out/; \
	  cd dist && zip -qr workbuddy_$(VERSION)_$${os}_$${arch}.zip workbuddy_$(VERSION)_$${os}_$${arch} && cd ..; \
	  echo "built $$out"; \
	done

# Tag a new release (usage: make tag VERSION=0.8.0).
tag:
	git tag -a v$(VERSION) -m "v$(VERSION)"
	git push origin v$(VERSION)
