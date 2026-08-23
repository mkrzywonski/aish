#!/usr/bin/env bash
# install.sh -- install aish (and optionally aishwnd, the Linux/WSL half of
# the aishwin feature) on this machine.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash
#   ./install.sh [options]
#
# By default this tries a prebuilt GitHub release binary first, falling back
# to building from source (installing Go if needed) if no prebuilt binary
# fits this machine or the download fails. No prompts: everything is a flag,
# so the one-liner above works with no attached terminal.
#
# Options:
#   --source            skip the prebuilt attempt; always build from source
#   --prebuilt          only try the prebuilt binary; error instead of falling back
#   --prefix DIR         install prefix; binaries land in DIR/bin (default: /usr/local)
#   --user               shortcut for --prefix "$HOME/.local" (no sudo needed)
#   --components LIST    comma-separated: aish,aishwnd (default: aish,aishwnd)
#   --version TAG         a specific release tag instead of the latest
#   --go-version VER      Go tarball version to install from go.dev if needed (default: 1.25.5)
#   --update-rc           append the PATH export to your shell rc if it's missing (default: just print it)
#   -h, --help            show this help
set -euo pipefail

REPO="mkrzywonski/aish"
PREFIX="/usr/local"
COMPONENTS="aish,aishwnd"
VERSION="latest"
GO_VERSION="1.25.5"
MODE="auto"   # auto | source | prebuilt
UPDATE_RC=0

log()  { printf '%s\n' "$*" >&2; }
die()  { log "install.sh: error: $*"; exit 1; }

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
	case "$1" in
	--source) MODE="source" ;;
	--prebuilt) MODE="prebuilt" ;;
	--prefix) PREFIX="$2"; shift ;;
	--user) PREFIX="$HOME/.local" ;;
	--components) COMPONENTS="$2"; shift ;;
	--version) VERSION="$2"; shift ;;
	--go-version) GO_VERSION="$2"; shift ;;
	--update-rc) UPDATE_RC=1 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
	shift
done

BINDIR="$PREFIX/bin"

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported CPU architecture: $(uname -m) (aish ships amd64/arm64 Linux binaries only)" ;;
	esac
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	*) die "aish is Linux-only (Linux-only PTY/termios + /proc). On Windows, run this inside WSL2; see README.md" ;;
	esac
}

# compute_sudo sets SUDO_CMD to "sudo" if $1 isn't writable by us and we're
# not root, else "" -- run in the current shell (not a subshell), because a
# die() called from inside a $(...) substitution would only kill that
# subshell and silently let the rest of the script carry on.
SUDO_CMD=""
compute_sudo() {
	dir="$1"
	SUDO_CMD=""
	[ "$(id -u)" -eq 0 ] && return
	mkdir -p "$dir" 2>/dev/null || true
	if [ -w "$dir" ] || { [ ! -e "$dir" ] && [ -w "$(dirname "$dir")" ]; }; then
		return
	fi
	command -v sudo >/dev/null 2>&1 || die "$dir isn't writable and sudo isn't available; rerun with --user or --prefix"
	SUDO_CMD="sudo"
}

install_binary() {
	src="$1" name="$2"
	compute_sudo "$BINDIR"
	# shellcheck disable=SC2086 -- SUDO_CMD is intentionally either empty or "sudo"
	$SUDO_CMD mkdir -p "$BINDIR"
	$SUDO_CMD install -m 755 "$src" "$BINDIR/$name"
	log "installed $BINDIR/$name -> $("$BINDIR/$name" version 2>/dev/null || "$BINDIR/$name" --version 2>/dev/null || echo "(installed)")"
}

install_prebuilt() {
	os=$(detect_os) arch=$(detect_arch)
	tmpdir=$(mktemp -d)
	trap 'rm -rf "$tmpdir"' RETURN
	ok=1
	for component in $(echo "$COMPONENTS" | tr ',' ' '); do
		url="https://github.com/$REPO/releases/$VERSION/download/${component}_${os}_${arch}.tar.gz"
		log "downloading $url"
		if ! curl -fsSL "$url" -o "$tmpdir/$component.tar.gz"; then
			log "no prebuilt $component for $os/$arch at $VERSION"
			ok=0
			continue
		fi
		tar -xzf "$tmpdir/$component.tar.gz" -C "$tmpdir" "$component"
		install_binary "$tmpdir/$component" "$component"
	done
	[ "$ok" -eq 1 ]
}

# ensure_go makes sure a `go` new enough to build aish is on PATH for the
# rest of this script (README.md notes distro Go packages are often too old
# for this project's go.mod requirement, so the reliable fallback is the
# same official go.dev tarball the README's manual instructions use).
ensure_go() {
	if command -v go >/dev/null 2>&1; then
		have=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
		req_major_minor=$(echo "$GO_VERSION" | cut -d. -f1,2)
		have_major_minor=$(echo "$have" | cut -d. -f1,2)
		if [ -n "$have" ] && [ "$(printf '%s\n%s\n' "$req_major_minor" "$have_major_minor" | sort -V | head -1)" = "$req_major_minor" ]; then
			log "using existing go $have"
			return
		fi
		log "existing go $have is older than $GO_VERSION; installing a fresh copy"
	fi

	arch=$(detect_arch)
	godir="$PREFIX/go"
	tmpdir=$(mktemp -d)
	tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
	log "downloading https://go.dev/dl/$tarball"
	curl -fsSL "https://go.dev/dl/$tarball" -o "$tmpdir/$tarball"
	compute_sudo "$PREFIX"
	# shellcheck disable=SC2086
	$SUDO_CMD rm -rf "$godir"
	# shellcheck disable=SC2086
	$SUDO_CMD tar -C "$PREFIX" -xzf "$tmpdir/$tarball"
	rm -rf "$tmpdir"
	export PATH="$godir/bin:$PATH"
	command -v go >/dev/null 2>&1 || die "go install to $godir didn't land on PATH"
	log "installed go $(go env GOVERSION) to $godir (add $godir/bin to PATH to keep using it directly)"
}

# ensure_repo makes $REPO_DIR a checkout with this project's go.mod --
# reusing the current directory if install.sh is already being run from
# inside one (the common case for a contributor), else cloning a fresh copy.
ensure_repo() {
	if [ -f go.mod ] && grep -q '^module ai-ssh$' go.mod 2>/dev/null; then
		REPO_DIR="$PWD"
		return
	fi
	command -v git >/dev/null 2>&1 || die "git is required to build from source"
	REPO_DIR=$(mktemp -d)
	log "cloning https://github.com/$REPO.git to $REPO_DIR"
	git clone --depth 1 "https://github.com/$REPO.git" "$REPO_DIR"
}

install_from_source() {
	ensure_go
	ensure_repo
	for component in $(echo "$COMPONENTS" | tr ',' ' '); do
		log "building $component"
		(cd "$REPO_DIR" && go build -o "$component" "./cmd/$component")
		install_binary "$REPO_DIR/$component" "$component"
	done
}

check_path() {
	case ":$PATH:" in
	*":$BINDIR:"*) return ;;
	esac
	export_line="export PATH=\"$BINDIR:\$PATH\""
	if [ "$UPDATE_RC" -eq 1 ]; then
		rc="$HOME/.bashrc"
		[ -n "${ZSH_VERSION:-}" ] && rc="$HOME/.zshrc"
		printf '\n# added by aish install.sh\n%s\n' "$export_line" >>"$rc"
		log "$BINDIR isn't on PATH; added it to $rc (open a new shell, or: source $rc)"
	else
		log "$BINDIR isn't on PATH; add this to your shell rc file (or rerun with --update-rc):"
		log "  $export_line"
	fi
}

main() {
	case "$MODE" in
	prebuilt)
		install_prebuilt || die "prebuilt install failed"
		;;
	source)
		install_from_source
		;;
	auto)
		if ! install_prebuilt; then
			log "falling back to building from source"
			install_from_source
		fi
		;;
	esac
	check_path
	log "done."
}

main
