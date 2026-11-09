package webui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The Store learns everything from log lines, matched by message. A message
// renamed at its emitter would silently empty a dashboard panel, so every
// message Observe and findingMsgs read must still be emitted as a literal by
// the code outside this package.
func TestStoreReadsOnlyEmittedMessages(t *testing.T) {
	read := map[string]bool{}
	for m := range findingMsgs {
		read[m] = true
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Observe" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, _ := strconv.Unquote(lit.Value)
						read[s] = true
					}
				}
			}
			return true
		})
		return false
	})
	if len(read) < 20 {
		t.Fatalf("found only %d messages the Store reads; the parse missed Observe", len(read))
	}

	emitted := map[string]bool{}
	root := filepath.Join("..", "..")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "webui" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		pf, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(pf, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					emitted[s] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for m := range read {
		if !emitted[m] {
			t.Errorf("the Store reads %q, but no code outside webui emits it", m)
		}
	}
}
