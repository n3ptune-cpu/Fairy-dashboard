package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Second review of audit 5's round 2 (note 6): no test noticed when update() took no lock (with the lock taken
// out of it, `go test ./cmd/fairy` passed; TestOneUpdateAtATime tests the lock itself). update() cannot be run
// by a test: it needs root, the release key and the release host, and with a mistake in it a test run as root
// on a panel's machine would start a real update. So this reads update() as the compiler does and holds the
// order it must keep: the lock is taken, and let go of only when update() returns, before anything is looked
// at or changed that a second run could also touch (a stopped run's leftovers, the address check, the
// download and the swap).
func TestUpdateTakesItsLockBeforeItTouchesAnything(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "update.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if x, ok := d.(*ast.FuncDecl); ok && x.Name.Name == "update" && x.Recv == nil {
			fn = x
		}
	}
	if fn == nil {
		t.Fatal("update.go has no func update")
	}
	name := func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				return id.Name + "." + x.Sel.Name
			}
		}
		return ""
	}
	first := map[string]token.Pos{} // where each call first stands in update()
	var unlockVar string
	deferred := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt: // unlock, err := lockUpdate(...)
			if len(x.Rhs) == 1 {
				if c, ok := x.Rhs[0].(*ast.CallExpr); ok && name(c.Fun) == "lockUpdate" && len(x.Lhs) == 2 {
					unlockVar = name(x.Lhs[0])
				}
			}
		case *ast.DeferStmt:
			deferred[name(x.Call.Fun)] = true
		case *ast.CallExpr:
			if n := name(x.Fun); n != "" {
				if _, seen := first[n]; !seen {
					first[n] = x.Pos()
				}
			}
		}
		return true
	})
	lock, ok := first["lockUpdate"]
	if !ok || unlockVar == "" || unlockVar == "_" {
		t.Fatal("update() does not take the update lock (lockUpdate), or throws its unlock away")
	}
	if !deferred[unlockVar] {
		t.Fatalf("update() does not hold the lock until it returns (no `defer %s()`)", unlockVar)
	}
	for _, after := range []string{"clearStoppedRun", "configStops", "healthAddr", "healthPreflight", "cli.Run", "setupPanelButton"} {
		pos, ok := first[after]
		if !ok {
			t.Fatalf("update() no longer calls %s: this test must follow the change", after)
		}
		if pos < lock {
			t.Fatalf("update() calls %s (%s) before it takes the lock (%s)", after, fset.Position(pos), fset.Position(lock))
		}
	}
}
