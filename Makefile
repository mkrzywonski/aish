# Development builds.
#
# Both shipping paths already inject their own version at link time
# (.goreleaser.yaml from the git tag, package.nix from its rev), so the
# `var version` constant in cmd/aish/main.go is only ever the fallback for a
# build that injects nothing — notably `go install ./cmd/aish`, which is what
# the README tells people to run. Bump that constant per RELEASE.
#
# Every build in between takes its identity from git instead:
#
#   v0.4.0                     exactly the tagged release
#   v0.4.0-3-gabc1234          three commits past the tag
#   v0.4.0-3-gabc1234-dirty    ...and built from a modified tree
#
# The -dirty suffix is the point. It distinguishes a binary built from
# uncommitted changes from one built clean at the same commit — something a
# hand-maintained version number cannot express, and the exact confusion that
# arises when two different builds both call themselves 0.4.0.
#
# Go is not on PATH on the NixOS box; shell.nix provides it:
#
#   nix-shell --run "make"
#   nix-shell --run "make check"
#
# In WSL, plain `make` works.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test vet check fmt version clean

build:
	go build -ldflags "$(LDFLAGS)" -o aish ./cmd/aish

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/aish

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
	rm -f aish
