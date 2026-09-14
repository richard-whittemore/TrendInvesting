package floatingpointaudit

import (
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCgoSources(t *testing.T) {
	if !build.Default.CgoEnabled {
		t.Skip("cgo sources are inactive in this build")
	}
	audit, err := os.ReadFile("audit_test.go")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, scalar, body string
		mixed, wantFailure bool
	}{
		{"cgo_only_float", "float64", "return x + a*b", false, true},
		{"mixed_c_double", "C.double", "x += a*b; return x", true, true},
		{"rounded_c_double", "C.double", "return x + C.double(a*b)", false, false},
		{"integer_c_int", "C.int", "return x + a*b", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"go.mod": "module fixture\n\ngo 1.24.4\n",
				"internal/floatingpointaudit/audit_test.go": string(audit),
				"internal/plain/plain.go":                   "package plain\n",
				"internal/cgofixture/shape.go": "package cgofixture\nimport \"C\"\n" +
					"func f(x, a, b " + tt.scalar + ") " + tt.scalar + " { " + tt.body + " }\n",
			}
			if tt.mixed {
				files["internal/cgofixture/plain.go"] = "package cgofixture\n"
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
			if tt.wantFailure {
				if err == nil || !strings.Contains(string(output), "internal/cgofixture/shape.go:3:") ||
					!strings.Contains(string(output), "fusible floating-point multiply-add") {
					t.Fatalf("expected source-located cgo fusion rejection, got %v:\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("safe cgo arithmetic rejected: %v\n%s", err, output)
			}
		})
	}
}
