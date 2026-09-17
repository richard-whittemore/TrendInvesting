package transport

import (
	"os"
	"strings"
	"testing"
)

func TestSocketDirectoryRequiresExpectedOwner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketDirectory(dir, os.Geteuid()); err != nil {
		t.Fatalf("current owner rejected: %v", err)
	}
	// ADR 0014: use a different expected UID against real directory metadata;
	// no privilege to chown or impersonate another account is required.
	err := prepareSocketDirectory(dir, os.Geteuid()+1)
	if err == nil || !strings.Contains(err.Error(), "owner UID") {
		t.Fatalf("want owner UID refusal, got %v", err)
	}
}
