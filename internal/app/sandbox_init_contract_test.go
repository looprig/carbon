package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPackageTestMainInitializesSandboxFirst(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate sandbox initialization contract test")
	}
	source := filepath.Join(filepath.Dir(filename), "subagent_acp_peer_test.go")
	file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}

	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "TestMain" {
			continue
		}
		if function.Body == nil || len(function.Body.List) == 0 {
			t.Fatal("TestMain has no body")
		}
		expression, ok := function.Body.List[0].(*ast.ExprStmt)
		if !ok {
			t.Fatal("TestMain must call sandbox.Init as its first statement")
		}
		call, ok := expression.X.(*ast.CallExpr)
		if !ok {
			t.Fatal("TestMain must call sandbox.Init as its first statement")
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Init" {
			t.Fatal("TestMain must call sandbox.Init as its first statement")
		}
		packageName, ok := selector.X.(*ast.Ident)
		if !ok || packageName.Name != "sandbox" || len(call.Args) != 0 {
			t.Fatal("TestMain must call sandbox.Init as its first statement")
		}
		return
	}

	t.Fatal("TestMain not found")
}
