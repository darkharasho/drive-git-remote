# Local builds mirror what .github/workflows/release.yml does, so a binary you
# build by hand reports the same version string as a released one.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/darkharasho/drive-git-remote/internal/cli.Version=$(VERSION)
PREFIX  ?= $(HOME)/.local/bin

.PHONY: build test check install release-snapshot clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o drive-git ./cmd/drive-git

# Everything CI runs, in the order CI runs it.
check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go build ./...
	go test ./... -timeout 10m

test:
	go test ./... -timeout 10m

# Tests that talk to real Drive using the logged-in account. Skipped by default.
test-live:
	DRIVE_GIT_LIVE=1 go test ./internal/drive/ -v

install: build
	install -d $(PREFIX)
	install -m 0755 drive-git $(PREFIX)/drive-git
	$(PREFIX)/drive-git install-helper --dir $(PREFIX)

# Keep in step with the target list in .github/workflows/release.yml.
TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

# Build every release target without publishing, to check a tag will work.
release-snapshot:
	rm -rf dist && mkdir -p dist
	@for t in $(TARGETS); do \
		goos=$${t%/*}; goarch=$${t#*/}; \
		name=drive-git; [ "$$goos" = "windows" ] && name=drive-git.exe; \
		echo "==> $$goos/$$goarch"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath \
			-ldflags "$(LDFLAGS)" -o "dist/$$name" ./cmd/drive-git || exit 1; \
		tar -czf "dist/drive-git_$(VERSION)_$${goos}_$${goarch}.tar.gz" -C dist "$$name"; \
		rm -f "dist/$$name"; \
	done
	@ls -la dist

clean:
	rm -rf dist drive-git
