package floatingpointaudit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceScope keeps test expectations and module entry points under the
// same rounding rule as domain code (docs/development.md; ADR 0017).
func TestSourceScope(t *testing.T) {
	audit, err := os.ReadFile("audit_test.go")
	if err != nil {
		t.Fatal(err)
	}
	locations := []struct{ name, dir, filename, pkg string }{
		{"internal_test", "internal/fixture", "shape_test.go", "fixture"},
		{"external_test", "internal/fixture", "shape_test.go", "fixture_test"},
		{"test_only", "internal/testonly", "shape_test.go", "testonly"},
		{"cmd", "cmd/fixture", "shape.go", "main"},
		{"cmd_test", "cmd/fixture", "shape_test.go", "main"},
		{"transport", "transport", "shape.go", "transport"},
		{"spike", "transport/spike", "shape.go", "spike"},
	}
	shapes := []struct {
		name, body string
		reject     bool
	}{
		{"add", "return x + a*b", true},
		{"accumulate", "x += a*b; return x", true},
		{"rounded", "x += float64(a*b); return x", false},
	}
	for _, location := range locations {
		for _, shape := range shapes {
			t.Run(location.name+"/"+shape.name, func(t *testing.T) {
				root := t.TempDir()
				source := location.dir + "/" + location.filename
				files := map[string]string{
					"go.mod": "module fixture\n\ngo 1.24.4\n",
					"internal/floatingpointaudit/audit_test.go": string(audit),
					"internal/fixture/internal_test.go":         "package fixture\nconst testValue = 1\n",
					"internal/fixture/plain.go":                 "package fixture\nconst Value = 1\n",
					source:                                      "package " + location.pkg + "\nimport base \"fixture/internal/fixture\"\nfunc f(x, a, b float64) float64 { _ = base.Value; " + shape.body + " }\n",
				}
				// Same-package fixtures need no self-import; external tests exercise
				// go list's remapping to the package compiled with its internal tests.
				if location.pkg == "fixture" {
					files[source] = "package fixture\nfunc f(x, a, b float64) float64 { " + shape.body + " }\n"
				}
				for name, content := range files {
					path := filepath.Join(root, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command("go", "test", "./internal/floatingpointaudit", "-run", "^TestNoFusibleMultiplyAdd$", "-count=1")
				cmd.Dir = root
				output, err := cmd.CombinedOutput()
				if shape.reject {
					if err == nil || !strings.Contains(string(output), source+":") || !strings.Contains(string(output), "fusible floating-point multiply-add") {
						t.Fatalf("expected source-located fusion rejection, got %v:\n%s", err, output)
					}
					t.Logf("guard rejected %s:\n%s", source, output)
				} else if err != nil {
					t.Fatalf("rounded arithmetic rejected: %v\n%s", err, output)
				}
			})
		}
	}
}
