# Development builds.
#
# Both shipping paths already inject their own version at link time
# (.goreleaser.yaml from the git tag, package.nix from its rev), so the
# `var version` constant in cmd/aish/main.go is the fallback for a build that
# injects nothing. At runtime, those builds derive a `g<revision>[-dirty]`
# identity from Go's embedded VCS metadata.
#
# Every build in between takes its identity from git instead:
#
#   v0.2.2                     exactly the tagged release
#   v0.2.2-3-gabc1234          three commits past the tag
#   v0.2.2-3-gabc1234-dirty    ...and built from a modified tree
#
# The -dirty suffix is the point. It distinguishes a binary built from
# uncommitted changes from one built clean at the same commit — something a
# hand-maintained version number cannot express, and the exact confusion that
# arises when two different builds report the same release number.
#
# Go is not on PATH on the NixOS box; shell.nix provides it:
#
#   nix-shell --run "make"
#   nix-shell --run "make check"
#
# In WSL, plain `make` works.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# One install location, on purpose. Two copies on different PATH precedences —
# a login shell finding one and the MCP proxy's non-login shell finding another
# — means sessions and proxy can silently run different builds.
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

.PHONY: build install aishwin aishwin-dev aishwnd install-aishwnd test vet check fmt version clean

build:
	go build -ldflags "$(LDFLAGS)" -o aish ./cmd/aish

# aishwnd is the Linux/WSL half of the aishwin feature — installs alongside
# aish so `wsl.exe -- aishwnd` (aishwin.exe's default launch path) finds it
# on PATH.
aishwnd:
	go build -ldflags "$(LDFLAGS)" -o aishwnd ./cmd/aishwnd

# aishwin.exe is the Windows half — cross-compiled, copy it to the Windows
# machine (there's no `make install` target for it; see README/plan doc for
# the manual copy step).
aishwin:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o aishwin.exe ./cmd/aishwin

# aishwin-dev is a DISTINCT build variant carrying the aishwindev tag: it
# auto-approves the human approval dialog and activates a file-triggered
# remote-control channel (devctl.go) so the AI can drive menu items and
# dialogs unattended while iterating on the GUI itself. devBuild is a
# compile-time fact (see cmd/aishwin/devmode.go) baked into this binary --
# an ordinary `make aishwin` build never carries the tag and can never
# accidentally skip approval. Never install this as the real session's
# aishwin.exe; its window title says "[DEV]" for exactly this reason.
aishwin-dev:
	GOOS=windows GOARCH=amd64 go build -tags aishwindev -ldflags "$(LDFLAGS)" -o aishwin-dev.exe ./cmd/aishwin

# Usage:  make build && sudo make install
#
# Deliberately does NOT depend on build: building under sudo would produce a
# root-owned binary, and `git describe` run as root trips git's dubious-ownership
# check, silently stamping the binary "dev" instead of the real version. Build as
# yourself, install as root.
install:
	@test -f aish || { echo "no ./aish — run 'make build' as your own user first, so the version stamp is right"; exit 1; }
	install -m 755 aish $(DESTDIR)$(BINDIR)/aish
	@echo "installed $(DESTDIR)$(BINDIR)/aish -> $$($(DESTDIR)$(BINDIR)/aish version)"

# Separate from install: aishwnd needs to land on PATH (same one-location
# rule as aish, since `wsl.exe -- aishwnd`, aishwin.exe's default launch path,
# resolves PATH the way a non-interactive WSL invocation does) but is kept
# as its own step rather than folded into the primary aish install path.
install-aishwnd:
	@test -f aishwnd || { echo "no ./aishwnd — run 'make aishwnd' as your own user first, so the version stamp is right"; exit 1; }
	install -m 755 aishwnd $(DESTDIR)$(BINDIR)/aishwnd
	@echo "installed $(DESTDIR)$(BINDIR)/aishwnd -> $$($(DESTDIR)$(BINDIR)/aishwnd --version)"

test:
	go test ./...

vet:
	go vet ./...

# What to run before committing.
check: vet test

# Reports files needing gofmt without rewriting them.
fmt:
	@gofmt -l internal cmd

# Print the version this build would stamp, without building.
version:
	@echo $(VERSION)

clean:
	rm -f aish aishwnd aishwin.exe
