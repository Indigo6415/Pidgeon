# Command names come from internal/brand/brand.go so renaming happens in one place.
MODULE  := github.com/Indigo6415/pidgeon
NAME    := $(shell sed -n 's/^[[:space:]]*Command = "\(.*\)"/\1/p' internal/brand/brand.go)
ALIAS   := $(shell sed -n 's/^[[:space:]]*Alias = "\(.*\)"/\1/p' internal/brand/brand.go)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/brand.Version=$(VERSION)
GOFLAGS := -trimpath
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# Static binaries: no libc dependency, copy anywhere and run.
export CGO_ENABLED := 0

.PHONY: build install uninstall run test vet cross clean

## build: compile pidgeon and pdgn into ./bin
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

## install: build and copy onto PATH ($$INSTALL_DIR, $$GOBIN, ~/go/bin, or /usr/local/bin)
install:
	@VERSION=$(VERSION) ./install.sh

## uninstall: remove the installed binaries from the same location install uses
uninstall:
	@dest="$${INSTALL_DIR:-$$(go env GOBIN)}"; dest="$${dest:-$$(go env GOPATH | cut -d: -f1)/bin}"; \
	rm -f "$$dest/$(NAME)" "$$dest/$(ALIAS)" && echo "removed $(NAME) and $(ALIAS) from $$dest"

## run: serve the current directory from source
run:
	go run ./cmd/$(NAME)

## test: run the test suite with the race detector (cgo is required for -race)
test:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

## cross: build release binaries for every platform into ./bin/dist
cross:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		out=bin/dist/$(NAME)_$${os}_$${arch}$$ext; \
		echo "→ $$out"; \
		GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $$out ./cmd/$(NAME) || exit 1; \
	done

clean:
	rm -rf bin
