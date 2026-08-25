package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTargetConfidenceForClassification(t *testing.T) {
	c := &Core{}
	cases := []struct {
		name string
		tool string
		via  string
		want string
	}{
		{"control tool reports its own", "session_status", "controlmaster", ""},
		{"terminal tool has no route", "run_command", "", ""},
		{"private auth tool", "oob_log", "", ""},
		{"routed tool that never resolved a route", "file_write", "", ""},
		{"local session is one machine", "file_write", "local", "same"},
		{"in-band types into the watched terminal", "file_write", "in_band", "same"},
		{"unrecognised route claims nothing", "file_write", "carrier_pigeon", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.targetConfidenceFor(tc.tool, tc.via); got != tc.want {
				t.Fatalf("targetConfidenceFor(%q, %q) = %q, want %q", tc.tool, tc.via, got, tc.want)
			}
		})
	}
}

// TestGuardTargetWarningIsNeverDiscarded is a source-level guard, not a
// behavioural test, because the bug it prevents is invisible to behavioural
// tests: `if _, err := c.guardTarget(rt, opMutate); err != nil` compiles,
// passes every test, and silently throws away the note telling the caller its
// target host could not be verified. Four of the six mutating call sites had
// drifted into exactly that shape, and nothing caught it -- the tools that
// could damage the wrong machine were the ones that said least about which
// machine they were on.
//
// The middleware now guarantees the machine-readable target_confidence
// regardless, so this guards the remaining half: the prose that says what to
// do about it.
func TestGuardTargetWarningIsNeverDiscarded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Rhs) != 1 {
				return true
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "guardTarget" {
				return true
			}
			found++
			if len(assign.Lhs) == 0 {
				return true
			}
			if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name == "_" {
				t.Errorf("%s: guardTarget's warning is discarded; give it to the caller "+
					"in the result's warning field so an unverified target is never silent",
					fset.Position(assign.Pos()))
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("no guardTarget call sites found; this guard has stopped guarding anything")
	}
}
