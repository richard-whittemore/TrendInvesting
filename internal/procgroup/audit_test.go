package procgroup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// killPrograms are the programs that send signals by name.
var killPrograms = map[string]bool{"kill": true, "pkill": true, "killall": true}

// guardedExec is the one function outside this package that may name a
// signalling program: the triage job's end of the gate user's processes,
// which refuses any target but the gate user's own UID (cmd/triage,
// docs/development.md, "Ending processes").
const guardedExec = "cmd/triage/gate.go:pkillGateUser"

// TestNoKillOutsideTheGuardedFunctions fails if any non-test Go source under
// cmd/, internal/ or transport/ calls kill(2) other than through Kill, names
// kill, pkill or killall other than in guardedExec, or passes "-1" (every
// process the caller may signal) beside such a program.
func TestNoKillOutsideTheGuardedFunctions(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, top := range []string{"cmd", "internal", "transport"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files++
			auditFile(t, p, filepath.ToSlash(rel))
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if files < 50 {
		t.Fatalf("read %d files; the walk did not find the sources", files)
	}
}

func auditFile(t *testing.T, p, rel string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	inProcgroup := path.Dir(rel) == "internal/procgroup"
	for _, decl := range f.Decls {
		where := rel
		if fn, ok := decl.(*ast.FuncDecl); ok {
			where = rel + ":" + fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && (x.Name == "syscall" || x.Name == "unix") && n.Sel.Name == "Kill" && !inProcgroup {
					t.Errorf("%s calls %s.Kill: end a process group with procgroup.Kill", where, x.Name)
				}
			case *ast.BasicLit:
				if v, ok := literal(n); ok && killPrograms[path.Base(v)] && where != guardedExec {
					t.Errorf("%s names %q: only %s may", where, v, guardedExec)
				}
			case *ast.CallExpr:
				var signals, everyone bool
				for _, a := range n.Args {
					if lit, ok := a.(*ast.BasicLit); ok {
						v, _ := literal(lit)
						signals = signals || killPrograms[path.Base(v)]
						everyone = everyone || v == "-1"
					}
				}
				if signals && everyone {
					t.Errorf("%s passes -1 to a signalling program: every process of the user", where)
				}
			}
			return true
		})
	}
}

// literal is a string literal's value.
func literal(l *ast.BasicLit) (string, bool) {
	if l.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(l.Value)
	return v, err == nil
}
