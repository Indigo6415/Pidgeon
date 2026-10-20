#!/bin/sh
# Install pidgeon (and its short alias pdgn) onto your PATH.
#
#   ./install.sh                     from a checkout: build and install
#   curl -fsSL <raw-url>/install.sh | sh
#                                    anywhere: `go install` the latest release
#
# Destination, first match wins:
#   $INSTALL_DIR  →  $GOBIN  →  $(go env GOPATH)/bin (usually ~/go/bin)
#   → /usr/local/bin if the above isn't writable (may need sudo).
#
# Requires Go 1.26+ (https://go.dev/dl/). The result is a single static
# binary with no runtime dependencies.
set -eu

MODULE="github.com/Indigo6415/pidgeon"
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || echo .)

# Read the command names from the one place they're defined, so a rename in
# internal/brand/brand.go flows through here automatically.
BRAND="$SCRIPT_DIR/internal/brand/brand.go"
if [ -f "$BRAND" ]; then
	NAME=$(sed -n 's/^[[:space:]]*Command = "\(.*\)"/\1/p' "$BRAND")
	ALIAS=$(sed -n 's/^[[:space:]]*Alias = "\(.*\)"/\1/p' "$BRAND")
else
	NAME=pidgeon
	ALIAS=pdgn
fi

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || die "Go is required: install it from https://go.dev/dl/ and re-run."

# Pick the destination directory.
if [ -n "${INSTALL_DIR:-}" ]; then
	DEST=$INSTALL_DIR
elif [ -n "$(go env GOBIN)" ]; then
	DEST=$(go env GOBIN)
else
	DEST="$(go env GOPATH | cut -d: -f1)/bin"
fi
mkdir -p "$DEST" 2>/dev/null || true
# Unwritable (e.g. a read-only GOPATH): fall back to /usr/local/bin.
if [ ! -w "$DEST" ]; then
	say "note: $DEST is not writable, falling back to /usr/local/bin"
	DEST=/usr/local/bin
	if [ ! -w "$DEST" ]; then
		die "$DEST needs root. Re-run with: sudo INSTALL_DIR=$DEST $0"
	fi
fi

export CGO_ENABLED=0
if [ -f "$SCRIPT_DIR/go.mod" ] && grep -q "^module $MODULE\$" "$SCRIPT_DIR/go.mod"; then
	# Building from a checkout: stamp the version from git.
	VERSION=${VERSION:-$(git -C "$SCRIPT_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)}
	say "building ${NAME} ${VERSION}…"
	(cd "$SCRIPT_DIR" && go build -trimpath \
		-ldflags "-s -w -X $MODULE/internal/brand.Version=$VERSION" \
		-o "$DEST/$NAME" "./cmd/$NAME")
else
	say "installing ${NAME} from ${MODULE}@latest…"
	GOBIN="$DEST" go install -trimpath -ldflags "-s -w" "$MODULE/cmd/$NAME@latest"
fi

# The alias is a symlink to the same binary.
ln -sf "$NAME" "$DEST/$ALIAS"
say "installed $DEST/$NAME (and $ALIAS)"

# Tell the user how to put DEST on PATH if it isn't already.
case ":$PATH:" in
*":$DEST:"*)
	say "run '$NAME' (or '$ALIAS') in any directory to share it."
	;;
*)
	say ""
	say "$DEST is not on your PATH yet. Add it with:"
	case "${SHELL:-}" in
	*/zsh) say "  echo 'export PATH=\"$DEST:\$PATH\"' >> ~/.zshrc && source ~/.zshrc" ;;
	*/fish) say "  fish_add_path $DEST" ;;
	*) say "  echo 'export PATH=\"$DEST:\$PATH\"' >> ~/.bashrc && source ~/.bashrc" ;;
	esac
	;;
esac
