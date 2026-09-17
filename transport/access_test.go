package transport_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/transport"
)

func TestListenSocketMode(t *testing.T) {
	for _, mask := range []string{"000", "022", "077"} {
		t.Run(mask, func(t *testing.T) {
			if os.Getenv("TRANSPORT_TEST_UMASK") != mask {
				cmd := exec.Command(os.Args[0], "-test.run=^TestListenSocketMode$/^"+mask+"$", "-test.v")
				cmd.Env = append(os.Environ(), "TRANSPORT_TEST_UMASK="+mask)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("umask subprocess: %v\n%s", err, output)
				}
				return
			}
			var value int
			if _, err := fmt.Sscanf(mask, "%o", &value); err != nil {
				t.Fatal(err)
			}
			// ADR 0014: isolate the process-wide umask from parallel tests.
			syscall.Umask(value)
			path := socketPath(t)
			server, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			assertMode(t, path, os.ModeSocket|0o600)
			assertMode(t, filepath.Dir(path), os.ModeDir|0o700)
		})
	}
}

func TestListenCreatesPrivateParent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(filepath.Dir(socketPath(t)), "private", "s.sock")
	server, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
	if err != nil {
		t.Fatalf("Listen with absent parent: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	assertMode(t, filepath.Dir(path), os.ModeDir|0o700)
	assertMode(t, path, os.ModeSocket|0o600)
}

func TestListenRefusesWideParent(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o755, 0o770, 0o777} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			path := socketPath(t)
			dir := filepath.Dir(path)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			server, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
			if err == nil {
				_ = server.Close()
				t.Fatalf("Listen accepted parent mode %04o", mode)
			}
			if !strings.Contains(err.Error(), "socket directory") {
				t.Errorf("want directory access error, got %v", err)
			}
			assertMode(t, dir, os.ModeDir|mode)
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Errorf("refusal must not bind a socket: %v", err)
			}
		})
	}
}

func TestListenRefusesSymlinkParent(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	link := filepath.Join(filepath.Dir(socketPath(t)), "link")
	if err := os.Symlink(filepath.Dir(path), link); err != nil {
		t.Fatal(err)
	}
	server, err := transport.Listen(filepath.Join(link, "s.sock"), echoDecider, transport.ServerConfig{})
	if err == nil {
		_ = server.Close()
		t.Fatal("Listen accepted symlink parent")
	}
	if !strings.Contains(err.Error(), "socket directory") {
		t.Errorf("want directory access error, got %v", err)
	}
}

func TestListenRequiresFilesystemPath(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", "@decision", "\x00decision"} {
		server, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
		if err == nil {
			_ = server.Close()
			t.Errorf("Listen accepted non-filesystem path %q", path)
		} else if !strings.Contains(err.Error(), "filesystem pathname") {
			t.Errorf("path %q: want filesystem pathname refusal, got %v", path, err)
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != want {
		t.Errorf("mode = %s (%04o), want %s (%04o)", info.Mode(), info.Mode().Perm(), want, want.Perm())
	}
}
