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

.PHONY: build install aicmd aicmdd install-aicmdd test vet check fmt version clean

build:
	go build -ldflags "$(LDFLAGS)" -o aish ./cmd/aish

# aicmdd is the Linux/WSL half of aicmd — installs alongside aish so
# `wsl.exe -- aicmdd` (aicmd.exe's default launch path) finds it on PATH.
aicmdd:
	go build -ldflags "$(LDFLAGS)" -o aicmdd ./cmd/aicmdd

# aicmd.exe is the Windows half — cross-compiled, copy it to the Windows
# machine (there's no `make install` target for it; see README/plan doc for
# the manual copy step).
aicmd:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o aicmd.exe ./cmd/aicmd

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

# Separate from install: aicmdd needs to land on PATH (same one-location
# rule as aish, since `wsl.exe -- aicmdd`, aicmd.exe's default launch path,
# resolves PATH the way a non-interactive WSL invocation does) but is kept
# as its own step rather than folded into the primary aish install path.
install-aicmdd:
	@test -f aicmdd || { echo "no ./aicmdd — run 'make aicmdd' as your own user first, so the version stamp is right"; exit 1; }
	install -m 755 aicmdd $(DESTDIR)$(BINDIR)/aicmdd
	@echo "installed $(DESTDIR)$(BINDIR)/aicmdd -> $$($(DESTDIR)$(BINDIR)/aicmdd --version)"

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
	rm -f aish aicmdd aicmd.exe
