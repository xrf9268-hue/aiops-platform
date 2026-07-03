package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestSleepContextUsesStoppableTimer(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "workflow_runtime.go", nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(workflow_runtime.go) = %v; want nil", err)
	}
	fn := findFunc(file, "sleepContext")
	if fn == nil {
		t.Fatal("sleepContext function not found")
	}
	if callsSelector(fn, "time", "After") {
		t.Fatal("sleepContext uses time.After; want time.NewTimer plus Stop")
	}
	if !callsSelector(fn, "time", "NewTimer") {
		t.Fatal("sleepContext does not call time.NewTimer")
	}
	if !callsStop(fn) {
		t.Fatal("sleepContext does not stop its timer")
	}
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func callsSelector(fn *ast.FuncDecl, receiver, selector string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != selector {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if ok && ident.Name == receiver {
			found = true
		}
		return true
	})
	return found
}

func callsStop(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "Stop" {
			found = true
		}
		return true
	})
	return found
}
