package procgroup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// killPrograms are the programs that send signals by name.
var killPrograms = map[string]bool{"kill": true, "pkill": true, "killall": true}

// guarded are the only declarations outside this package that may name a
// signalling program (docs/development.md, "Ending processes"): the triage
// job's end of the gate user's processes, which refuses any target but the
// gate user's own UID; its test, which hands it a recording stand-in sudo
// that runs nothing; and the stand-ins' refusal of every such program and
// its test.
var guarded = map[string]bool{
	"cmd/triage/gate.go:pkillGateUser":                                   true,
	"cmd/triage/gate_test.go:TestPkillGateUserEndsOnlyTheGateUsersOwn":   true,
	"cmd/triage/git_test.go:refuseSignals":                               true,
	"cmd/triage/git_test.go:TestTheStandInsRefuseEverySignallingProgram": true,
}

// selfFile is this audit, whose seeded sources name every violation.
const selfFile = "internal/procgroup/audit_test.go"

// TestNoKillOutsideTheGuardedFunctions fails if any Go source or shell
// script under cmd/, internal/ or transport/, tests included, calls kill(2)
// other than through Kill and WaitGone (syscall's or x/sys/unix's Kill, by any import
// name), or names kill, pkill or killall as a command (a path to one, one
// started by exec, or one followed by its arguments, in any string, a
// shell script's or `sh -c`'s included) outside guarded.
func TestNoKillOutsideTheGuardedFunctions(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the folder exemption for stand-ins, but audit the actual syscall
	// references in its production source (docs/development.md, "Ending processes").
	src, err := os.ReadFile(filepath.Join(root, "internal", "procgroup", "procgroup.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A path outside the exempt folder makes the same parser report all
	// syscall references, including aliases and function values.
	calls, err := auditSource("internal/procgroup.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/procgroup.go:Kill calls syscall.Kill: end a process group with procgroup.Kill",
		"internal/procgroup.go:WaitGone calls syscall.Kill: end a process group with procgroup.Kill",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("only Kill and WaitGone may call syscall.Kill; got %v", calls)
	}
	var files int
	for _, top := range []string{"cmd", "internal", "transport"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, ".sh") {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if rel == selfFile {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files++
			found, err := auditSource(rel, src)
			if err != nil {
				t.Errorf("%s: %v", rel, err)
			}
			for _, v := range found {
				t.Error(v)
			}
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

// TestTheAuditCatchesEverySeededKill: each source below reintroduces a
// signal the audit must catch (the exact regression, a test running
// `kill -KILL -1`, in its every form), and the last ones name the words in
// prose, which it must not.
func TestTheAuditCatchesEverySeededKill(t *testing.T) {
	head := "package x\n\nimport (\n\t\"os/exec\"\n\tsys \"syscall\"\n)\n\nvar _ = sys.Getpid\nvar _ = exec.Command\n\n"
	for name, c := range map[string]struct {
		rel, src string
		caught   bool
	}{
		"kill -1 in code":            {"cmd/x/x.go", head + "func f() { _ = exec.Command(\"/bin/kill\", \"-KILL\", \"-1\").Run() }\n", true},
		"kill -1 in a test":          {"cmd/x/x_test.go", head + "func TestF() { _ = exec.Command(\"/bin/kill\", \"-KILL\", \"-1\").Run() }\n", true},
		"syscall.Kill by an alias":   {"cmd/x/x_test.go", head + "func f() { _ = sys.Kill(-1, sys.SIGKILL) }\n", true},
		"syscall dot-imported":       {"cmd/x/x.go", "package x\n\nimport . \"syscall\"\n\nfunc f() { _ = Kill(-1, SIGKILL) }\n", true},
		"x/sys/unix":                 {"cmd/x/x.go", "package x\n\nimport \"golang.org/x/sys/unix\"\n\nfunc f() { _ = unix.Kill(-1, 9) }\n", true},
		"sh -c kill -1":              {"cmd/x/x_test.go", head + "func f() { _ = exec.Command(\"sh\", \"-c\", \"sleep 1; kill -KILL -1\").Run() }\n", true},
		"a script a test writes":     {"cmd/x/x_test.go", head + "const body = \"#!/bin/sh\\nkill $$\\n\"\n", true},
		"bare kill started by exec":  {"cmd/x/x.go", head + "func f() { _ = exec.Command(\"kill\").Run() }\n", true},
		"an argv of kill":            {"cmd/x/x_test.go", head + "var argv = []string{\"pkill\", \"-U\", \"501\"}\n", true},
		"killall by name":            {"cmd/x/x_test.go", head + "const s = \"killall Dock\"\n", true},
		"a shell script":             {"cmd/x/testdata/stop.sh", "#!/bin/sh\nkill -KILL -1\n", true},
		"a guarded name elsewhere":   {"cmd/x/x.go", head + "func pkillGateUser() { _ = exec.Command(\"/usr/bin/pkill\", \"-U\", \"1\") }\n", true},
		"prose: the kill switch":     {"cmd/x/x_test.go", head + "const s = \"the kill switch is engaged\"\nvar e = []string{\"kill\", \"kill-switch\"}\n", false},
		"a process the test started": {"cmd/x/x_test.go", head + "func f(c *exec.Cmd) { _ = c.Process.Kill() }\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			found, err := auditSource(c.rel, []byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			if c.caught && len(found) == 0 {
				t.Errorf("the audit did not catch:\n%s", c.src)
			}
			if !c.caught && len(found) != 0 {
				t.Errorf("the audit caught prose: %q", found)
			}
		})
	}
}

// auditSource is the violations in one file, rel its path from the
// repository's root.
func auditSource(rel string, src []byte) ([]string, error) {
	if strings.HasSuffix(rel, ".sh") {
		var found []string
		for i, line := range strings.Split(string(src), "\n") {
			if namesSignaller(line) {
				found = append(found, rel+":"+strconv.Itoa(i+1)+" names a signalling program")
			}
		}
		return found, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	inProcgroup := path.Dir(rel) == "internal/procgroup"
	// syscalls are the names syscall and x/sys/unix are imported under.
	syscalls := map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != "syscall" && p != "golang.org/x/sys/unix" {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." && !inProcgroup {
			found = append(found, rel+" dot-imports "+p+", whose Kill could then be called unseen")
		}
		syscalls[name] = true
	}
	for _, decl := range f.Decls {
		where := rel
		switch d := decl.(type) {
		case *ast.FuncDecl:
			where = rel + ":" + d.Name.Name
		case *ast.GenDecl:
			if len(d.Specs) > 0 {
				if v, ok := d.Specs[0].(*ast.ValueSpec); ok && len(v.Names) > 0 {
					where = rel + ":" + v.Names[0].Name
				}
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && syscalls[x.Name] && n.Sel.Name == "Kill" && !inProcgroup {
					found = append(found, where+" calls "+x.Name+".Kill: end a process group with procgroup.Kill")
				}
			case *ast.BasicLit:
				if v, ok := literal(n); ok && namesSignaller(v) && !guarded[where] {
					found = append(found, where+" names a signalling program in "+strconv.Quote(v))
				}
			case *ast.CallExpr:
				if !guarded[where] && signallingArgs(literals(n.Args), startsAProgram(n)) {
					found = append(found, where+" passes a signalling program and its arguments")
				}
			case *ast.CompositeLit:
				if !guarded[where] && signallingArgs(literals(n.Elts), false) {
					found = append(found, where+" builds a signalling program's command line")
				}
			}
			return true
		})
	}
	return found, nil
}

// namesSignaller reports whether text names kill, pkill or killall as a
// command: a path to one, or one followed by an argument (for kill, an
// option, a pid or a variable, so the prose "kill switch" is not one), in
// a shell line or anywhere in a string.
func namesSignaller(text string) bool {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(";|&()`'\"<>{}", r)
	})
	for i, w := range words {
		base := path.Base(w)
		if !killPrograms[base] {
			continue
		}
		if strings.Contains(w, "/") {
			return true
		}
		if i+1 < len(words) && (base != "kill" || commandArgument(words[i+1])) {
			return true
		}
	}
	return false
}

// commandArgument is a word that reads as kill's argument: an option, a pid
// or a variable.
func commandArgument(w string) bool {
	return w != "" && (w[0] == '-' || w[0] == '$' || w[0] >= '0' && w[0] <= '9')
}

// signallingArgs reports whether a call's or a list's string arguments are
// a signalling program's command line: one of them is the program, and it
// is started by exec (started) or followed by an argument.
func signallingArgs(args []string, started bool) bool {
	for i, a := range args {
		if !killPrograms[path.Base(a)] {
			continue
		}
		if started && i == 0 || i+1 < len(args) && commandArgument(args[i+1]) {
			return true
		}
	}
	return false
}

// startsAProgram reports whether call starts the program its first string
// argument names: exec.Command, exec.CommandContext, exec.LookPath or
// os.StartProcess.
func startsAProgram(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && slices.Contains([]string{"Command", "CommandContext", "LookPath", "StartProcess"}, sel.Sel.Name)
}

// literals is the string literals among exprs, in order.
func literals(exprs []ast.Expr) []string {
	var out []string
	for _, e := range exprs {
		if lit, ok := e.(*ast.BasicLit); ok {
			if v, ok := literal(lit); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// literal is a string literal's value.
func literal(l *ast.BasicLit) (string, bool) {
	if l.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(l.Value)
	return v, err == nil
}

func TestEndingProcessesDocumentsBothCallers(t *testing.T) {
	raw, err := os.ReadFile("../../docs/development.md")
	if err != nil {
		t.Fatal(err)
	}
	section := strings.Split(strings.Split(string(raw), "## Ending processes")[1], "\n## ")[0]
	text := strings.Join(strings.Fields(section), " ")
	for _, want := range []string{"`procgroup.Kill` and `procgroup.WaitGone`", "only probes with signal 0", "only kill(2) callers"} {
		if !strings.Contains(text, want) {
			t.Errorf("Ending processes lacks %q", want)
		}
	}
}
