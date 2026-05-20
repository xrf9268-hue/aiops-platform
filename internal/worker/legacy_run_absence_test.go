package worker_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerPackageDoesNotExposeLegacyQueueRunLoop(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read worker package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == "Run" {
				t.Fatalf("legacy queue worker Run loop still exists in %s; cmd/worker should dispatch through the orchestrator actor path", path)
			}
		}
	}
}
