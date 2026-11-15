package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestServeErrorsExitThroughFatalDrain pins R1-43: both serve failures that
// happen after the store exists (the port claim and a mid-life Serve error)
// must exit through fatalDrain, the one fatal exit that drains first. A raw
// log.Fatalf skips the write-channel drain and final flush, so a buffered
// record is discarded on that route. The source is parsed rather than grepped
// so a stray mention in a comment cannot satisfy the assertion.
func TestServeErrorsExitThroughFatalDrain(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	fatals := map[string]int{}
	var serveErrClause *ast.CommClause
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			return node.Name.Name == "main"
		case *ast.SelectStmt:
			for _, clause := range node.Body.List {
				comm, ok := clause.(*ast.CommClause)
				if !ok {
					continue
				}
				assign, ok := comm.Comm.(*ast.AssignStmt)
				if !ok || len(assign.Rhs) != 1 {
					continue
				}
				if _, ok := assign.Rhs[0].(*ast.UnaryExpr); !ok {
					continue
				}
				if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name == "err" {
					serveErrClause = comm
				}
			}
		case *ast.CallExpr:
			formatLiteral := func() (string, bool) {
				if len(node.Args) == 0 {
					return "", true
				}
				lit, ok := node.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return "", true
				}
				format, err := strconv.Unquote(lit.Value)
				return format, err == nil
			}
			switch fun := node.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "fatalDrain" {
					if format, ok := formatLiteral(); ok && format == "serve: %v" {
						fatals["fatalDrain"]++
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "log" && fun.Sel.Name == "Fatalf" {
					if format, ok := formatLiteral(); ok && format == "serve: %v" {
						fatals["log.Fatalf"]++
					}
				}
			}
		}
		return true
	})

	if fatals["log.Fatalf"] != 0 {
		t.Errorf("%d serve failures still exit through log.Fatalf; want fatalDrain", fatals["log.Fatalf"])
	}
	if fatals["fatalDrain"] != 2 {
		t.Errorf("serve failures routed through fatalDrain = %d, want 2 (port claim and mid-life Serve error)", fatals["fatalDrain"])
	}
	if serveErrClause == nil {
		t.Fatal("serveErr select clause not found in main")
	}
	drained := false
	ast.Inspect(serveErrClause, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch ident.Name {
		case "fatalDrain":
			drained = true
		case "os.Exit":
			t.Error("serveErr clause still exits with a bare os.Exit next to fatalDrain")
		}
		return true
	})
	if !drained {
		t.Error("the mid-life serve error branch does not drain before exiting")
	}
}
