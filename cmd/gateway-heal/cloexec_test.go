package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The healer's lock and evidence files must not be inherited by the systemctl
// children it starts (server.md, Gateway healer).
func TestLockAndEvidenceFilesAreCloseOnExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := openNoFollow(path, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(fd) }()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("descriptor is inherited across exec")
	}
}
