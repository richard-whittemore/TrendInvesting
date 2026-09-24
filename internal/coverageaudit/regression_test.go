package coverageaudit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAuditFixture(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSuppliedProfileIgnoresPackagesOutsideAuditScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeAuditFixture(t, root, "internal/sample/sample.go", "package sample\nfunc f() {\n panic(\"unreachable\")\n}\n")
	path := writeAuditFixture(t, root, "coverage.out", "mode: atomic\n"+
		modulePath+"internal/sample/sample.go:3.2,3.22 1 0\n"+
		modulePath+"cmd/fabricated/main.go:1.1,1.2 1 0\n"+
		modulePath+"transport/fabricated.go:1.1,1.2 1 0\n"+
		modulePath+"internalish/fabricated.go:1.1,1.2 1 0\n")
	got := uncoveredBlocks(t, root, path)
	if len(got) != 1 || got[0].File != "internal/sample/sample.go" || got[0].Statement != `panic("unreachable")` {
		t.Fatalf("uncovered blocks = %+v, want only internal/sample/sample.go's panic", got)
	}
}

func TestSuppliedProfileRejectsPathsOutsideModule(t *testing.T) {
	t.Parallel()
	for _, count := range []string{"0", "1"} {
		t.Run("execution_count_"+count, func(t *testing.T) {
			t.Parallel()
			requireAuditFailure(t, "foreign-profile-"+count, "outside module github.com/richard-whittemore/TrendInvesting")
		})
	}
}

func TestSuppliedProfileRejectsNewerCoverageInputs(t *testing.T) {
	for _, name := range []string{"internal/sample/sample_test.go", "internal/sample/sample.go", "go.mod", "go.sum", "internal/sample/testdata/input.json"} {
		t.Run(name, func(t *testing.T) {
			requireAuditFailure(t, "stale-profile:"+name, "coverage profile is older than "+name+"; regenerate it")
		})
	}
}

func TestSuppliedProfileAcceptsFreshInputs(t *testing.T) {
	root, path := freshnessFixture(t, "internal/sample/sample_test.go", false)
	t.Setenv(profileEnv, path)
	if got := profile(t, root); got != path {
		t.Fatalf("profile = %q, want %q", got, path)
	}
}

func freshnessFixture(t *testing.T, name string, newer bool) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = writeAuditFixture(t, root, "coverage.out", "mode: count\n"+modulePath+"internal/sample/sample.go:3.2,3.12 1 0\n")
	input := writeAuditFixture(t, root, name, "package sample\nfunc TestNowCoversPreviouslyExcludedGuard() {}\n")
	old := time.Unix(1000, 0)
	recent := old.Add(time.Hour)
	inputTime, profileTime := old, recent
	if newer {
		inputTime, profileTime = recent, old
	}
	for file, stamp := range map[string]time.Time{input: inputTime, path: profileTime} {
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return root, path
}

func identicalGuards(t *testing.T, counts ...int) []block {
	t.Helper()
	root := t.TempDir()
	source := "package sample\nfunc f(guard bool) error {\n"
	profile := "mode: count\n"
	for i, count := range counts {
		source += " if guard { return nil }\n"
		// Reverse profile order to require source-order ordinals.
		profile = strings.Replace(profile, "mode: count\n", fmt.Sprintf("mode: count\n%sinternal/sample/sample.go:%d.11,%d.25 1 %d\n", modulePath, i+3, i+3, count), 1)
	}
	source += " return nil\n}\n"
	writeAuditFixture(t, root, "internal/sample/sample.go", source)
	path := writeAuditFixture(t, root, "coverage.out", profile)
	return uncoveredBlocks(t, root, path)
}

func TestIdenticalGuardsCannotExchangeCoverage(t *testing.T) {
	requireAuditFailure(t, "swapped-guards", "1 statement(s) in internal/ are executed by no test and are not in exclusions.json")
}

func TestEditsAboveExcludedGuardPreserveExclusion(t *testing.T) {
	for _, edit := range []struct {
		name, beforeFunction, beforeGuard string
	}{
		{name: "outside_function", beforeFunction: "// An unrelated comment.\n\nfunc other() { return }\n"},
		{name: "inside_function", beforeGuard: "// An unrelated comment.\n _ = 1\n"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			root := t.TempDir()
			blocks := func(beforeFunction, beforeGuard string) []block {
				source := "package sample\n" + beforeFunction + "func f() {\n" + beforeGuard + " panic(\"unreachable\")\n}\n"
				writeAuditFixture(t, root, "internal/sample/sample.go", source)
				line := 3 + strings.Count(beforeFunction+beforeGuard, "\n")
				path := writeAuditFixture(t, root, "coverage.out", fmt.Sprintf("mode: count\n%sinternal/sample/sample.go:%d.2,%d.22 1 0\n", modulePath, line, line))
				return uncoveredBlocks(t, root, path)
			}
			listed := blocks("", "")
			listed[0].Category = "unreachable-by-invariant"
			listed[0].Reason = "synthetic guard invariant"
			checkExclusions(t, blocks(edit.beforeFunction, edit.beforeGuard), listed)
		})
	}
}

func TestGroupedExclusionCountIsRejected(t *testing.T) {
	requireAuditFailure(t, "grouped-count", "count 2 groups blocks; list each occurrence separately")
}

func TestIdenticalGuardsDumpSeparately(t *testing.T) {
	blocks := identicalGuards(t, 0, 0)
	path := filepath.Join(t.TempDir(), "dump.json")
	writeDump(t, path, blocks)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list exclusionList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Exclusions) != 2 || list.Exclusions[0].key() == list.Exclusions[1].key() {
		t.Fatalf("dump must name both guards separately; got %s", raw)
	}
	for _, e := range list.Exclusions {
		if e.count() != 1 {
			t.Fatalf("dump grouped guards: %+v", e)
		}
	}
}

func TestNegativeExclusionCountFailsWithoutPanic(t *testing.T) {
	requireAuditFailure(t, "negative-count", "internal/sample/sample.go:0 (f, occurrence 1): count -1 is negative")
}

func TestDefaultAndExplicitSingleCountsMatch(t *testing.T) {
	for _, count := range []int{0, 1} {
		t.Run(fmt.Sprintf("count_%d", count), func(t *testing.T) {
			b := block{File: "internal/sample/sample.go", Function: "f", Statement: "return nil", Occurrence: 1,
				Category: "unreachable-by-invariant", Reason: "synthetic first guard invariant", Count: count}
			checkExclusions(t, []block{b}, []block{b})
		})
	}
}

func TestDefaultAndExplicitFirstOccurrencesMatch(t *testing.T) {
	var listed block
	if err := json.Unmarshal([]byte(`{"file":"internal/sample/sample.go","function":"f","statement":"return nil","category":"unreachable-by-invariant","reason":"synthetic guard invariant"}`), &listed); err != nil {
		t.Fatal(err)
	}
	uncovered := listed
	uncovered.Occurrence = 1
	checkExclusions(t, []block{uncovered}, []block{listed})
	checkExclusions(t, []block{listed}, []block{uncovered})
}

func requireAuditFailure(t *testing.T, scenario, want string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCoverageAuditFailure$", "-test.v")
	cmd.Env = append(os.Environ(), "COVERAGE_AUDIT_FAILURE="+scenario)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), want) || strings.Contains(string(out), "panic:") {
		t.Fatalf("audit failure = %v; want clean failure containing %q; output:\n%s", err, want, out)
	}
	t.Logf("expected audit failure:\n%s", out)
}

func TestCoverageAuditFailure(t *testing.T) {
	scenario := os.Getenv("COVERAGE_AUDIT_FAILURE")
	if scenario == "" {
		return
	}
	if scenario == "negative-count" {
		b := block{File: "internal/sample/sample.go", Function: "f", Statement: "return nil", Occurrence: 1,
			Category: "unreachable-by-invariant", Reason: "synthetic first guard invariant", Count: -1}
		checkExclusions(t, []block{b}, []block{b})
		return
	}
	if scenario == "grouped-count" {
		b := block{File: "internal/sample/sample.go", Function: "f", Statement: "return nil",
			Category: "unreachable-by-invariant", Reason: "synthetic guard invariant", Count: 2}
		checkExclusions(t, []block{b, b}, []block{b})
		return
	}
	if scenario == "swapped-guards" {
		listed := identicalGuards(t, 0, 0, 1)
		for i := range listed {
			listed[i].Category = "unreachable-by-invariant"
			listed[i].Reason = "synthetic guard invariant"
		}
		checkExclusions(t, identicalGuards(t, 0, 1, 0), listed)
		return
	}
	if name, ok := strings.CutPrefix(scenario, "stale-profile:"); ok {
		root, path := freshnessFixture(t, name, true)
		t.Setenv(profileEnv, path)
		profile(t, root)
		return
	}
	if count, ok := strings.CutPrefix(scenario, "foreign-profile-"); ok {
		root := t.TempDir()
		path := writeAuditFixture(t, root, "coverage.out", fmt.Sprintf("mode: count\nexample.com/foreign/internal/file.go:1.1,1.2 1 %s\n", count))
		uncoveredBlocks(t, root, path)
		return
	}
	t.Fatalf("unknown failure scenario %q", scenario)
}
