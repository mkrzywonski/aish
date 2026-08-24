//go:build !windows

package main

// aishwin is a Windows GUI binary: the rest of this package is guarded by
// //go:build windows. This stub keeps the package type-checkable on other
// platforms so `go vet ./...` and `go test ./...` — and therefore
// `make check` — still work from Linux, where aishwin is cross-compiled
// with `make aishwin` (GOOS=windows).

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "aishwin is Windows-only; cross-compile it with `make aishwin`")
	os.Exit(1)
}
