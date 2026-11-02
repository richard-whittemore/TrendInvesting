package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/gatewayheal"
)

type systemd struct {
	run         func(string, ...string) ([]byte, error)
	setDeadline func(time.Time)
}

var statusTimeForRun = func(at time.Time) time.Time { return at }

func (s systemd) SetDeadline(at time.Time) {
	if s.setDeadline != nil {
		s.setDeadline(at)
	}
}

func (s systemd) Act(action, unit string) error {
	if err := gatewayheal.ValidateAction(action, unit); err != nil {
		return err
	}
	_, err := s.run("systemctl", "--job-mode=fail", action, unit)
	return err
}
func (s systemd) Status(unit string) (gatewayheal.Status, error) {
	var out gatewayheal.Status
	raw, err := s.run("systemctl", "show", "--timestamp=us", "--no-pager", "-p", "ActiveState", "-p", "ExecMainCode", "-p", "ExecMainStatus", "-p", "InvocationID", "-p", "ExecMainExitTimestamp", "-p", "ExecMainStartTimestamp", unit)
	if err != nil {
		return out, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return out, errors.New("malformed systemd properties")
		}
		props[k] = v
	}
	out.Active = props["ActiveState"]
	out.Invocation = props["InvocationID"]
	out.Code, err = strconv.Atoi(props["ExecMainCode"])
	if err != nil {
		return out, err
	}
	out.Exit, err = strconv.Atoi(props["ExecMainStatus"])
	if err != nil {
		return out, err
	}
	if stamp := props["ExecMainExitTimestamp"]; stamp != "" {
		out.Finished, err = time.Parse("Mon 2006-01-02 15:04:05 MST", stamp)
		if err != nil {
			return out, err
		}
	}
	if !out.Finished.IsZero() {
		out.Finished = statusTimeForRun(out.Finished)
	}
	stamp := props["ExecMainStartTimestamp"]
	if stamp == "" && out.Invocation != "" {
		// A unit that has run reports its start; without it the evidence is
		// incomplete and must not read as time zero (server.md, Gateway healer).
		return out, fmt.Errorf("%s: missing ExecMainStartTimestamp for invocation %s", unit, out.Invocation)
	}
	if stamp != "" {
		out.Started, err = time.Parse("Mon 2006-01-02 15:04:05 MST", stamp)
		if err != nil {
			return out, err
		}
		out.Started = statusTimeForRun(out.Started)
	}
	return out, nil
}
func (s systemd) Lines(unit, invocation string) ([]string, error) {
	if invocation == "" {
		return nil, errors.New("missing journal invocation")
	}
	raw, err := s.run("journalctl", "--no-pager", "-o", "cat", "_SYSTEMD_UNIT="+unit, "_SYSTEMD_INVOCATION_ID="+invocation)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw), "\n"), nil
}

type files struct{ schedule, counter, gatewayEnv string }

func (f files) Read() (gatewayheal.Inputs, error) {
	var out gatewayheal.Inputs
	raw, err := regularRead(f.schedule)
	if err != nil {
		return out, err
	}
	out.Schedule = raw
	raw, err = regularRead(f.gatewayEnv)
	if err != nil {
		return out, err
	}
	out.GatewayEnv = raw
	raw, err = regularRead(f.counter)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	out.Counter = raw
	return out, err
}
func regularRead(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		// Name the file: a journal line that says only "no such file" cannot
		// be acted on. errors.Is(err, os.ErrNotExist) still holds.
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("not a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(f, 1024*1024+1))
}

type diskState struct {
	dir string
	uid uint32
}

func (s diskState) Lock() (func(), error) {
	info, err := os.Lstat(s.dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || stat.Uid != s.uid {
		return nil, errors.New("healer directory must be root-owned mode 0o700")
	}
	return lockFile(filepath.Join(s.dir, "lock"), s.uid, true, gatewayheal.ErrAnotherHealer)
}
func (s diskState) Load() (gatewayheal.State, error) {
	var state gatewayheal.State
	path := filepath.Join(s.dir, "last-restart.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != s.uid || info.Mode().Perm() != 0o600 {
		return state, errors.New("restart state must be root-owned mode 0o600")
	}
	raw, err := regularRead(path)
	if err != nil {
		return state, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return state, errors.New("restart state must be an object")
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return state, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return state, errors.New("duplicate restart state field")
		}
		seen[key] = true
		switch key {
		case "last_restart":
			err = dec.Decode(&state.LastRestart)
		case "incident":
			err = dec.Decode(&state.Incident)
		default:
			return state, fmt.Errorf("unknown restart state field %q", key)
		}
		if err != nil {
			return state, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return state, err
	}
	if state.LastRestart.IsZero() || state.Incident == "" {
		return state, errors.New("incomplete restart state")
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return state, errors.New("trailing restart state")
	}
	return state, nil
}
func (s diskState) Save(state gatewayheal.State) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".restart-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.dir, "last-restart.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// relayGuard holds the same flock inodes as adapter/ibkr/relay.py across the
// Gateway restart and login. A running relay, even another account's, blocks
// restarting Gateway (ADR 0026 point 8; docs/manual/server.md, Gateway healer).
type relayGuard struct{ root string }

func (g relayGuard) Acquire() (func(), error) {
	var releases []func()
	release := func() {
		for _, done := range releases {
			done()
		}
	}
	for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
		dir := filepath.Join(g.root, name)
		info, err := os.Lstat(dir)
		if err != nil {
			release()
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() {
			release()
			return nil, errors.New("invalid adapter directory")
		}
		done, err := lockFile(filepath.Join(dir, "pending.json.lock"), stat.Uid, false, gatewayheal.ErrOrderBusy)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, done)
		raw, err := regularRead(filepath.Join(dir, "pending.json.handover"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			release()
			return nil, err
		}
		if err == nil && !gatewayheal.HandoverClear(raw) {
			release()
			return nil, fmt.Errorf("%s: unsettled hand-over: %w", name, gatewayheal.ErrOrderBusy)
		}

	}
	return release, nil
}

func lockFile(path string, uid uint32, create bool, busy error) (func(), error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= syscall.O_CREAT
	}
	fd, err := syscall.Open(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	closeFile := func() { _ = f.Close() }
	info, err := f.Stat()
	if err != nil {
		closeFile()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || stat.Uid != uid {
		closeFile()
		return nil, fmt.Errorf("lock is not a regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeFile()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s held: %w", path, busy)
		}
		return nil, err
	}
	return closeFile, nil
}

// Loss reads this adapter invocation's uninterrupted connection loss (ADR 0025;
// cmd/reporter/gatewayjournal.go, lostSince). A restored link resets its origin.
func (s systemd) Loss(unit, invocation string) (time.Time, error) {
	if invocation == "" {
		return time.Time{}, errors.New("missing journal invocation")
	}
	raw, err := s.run("journalctl", "--no-pager", "-o", "short-iso-precise", "--grep=ibkr link state=connectivity-lost|ibkr link state=connected reason=restored|ibkr reconciled client_id=", "_SYSTEMD_UNIT="+unit, "_SYSTEMD_INVOCATION_ID="+invocation)
	if err != nil {
		return time.Time{}, err
	}
	var loss time.Time
	for _, line := range strings.Split(string(raw), "\n") {
		lost := strings.Contains(line, "ibkr link state=connectivity-lost")
		restored := strings.Contains(line, "ibkr link state=connected reason=restored") || strings.Contains(line, "ibkr reconciled client_id=")
		if !lost && !restored {
			continue
		}
		field, _, _ := strings.Cut(line, " ")
		at, err := time.Parse("2006-01-02T15:04:05-0700", field)
		if err != nil {
			at, err = time.Parse(time.RFC3339, field)
		}
		if err != nil {
			return time.Time{}, err
		}
		if restored {
			loss = time.Time{}
		} else if loss.IsZero() {
			loss = statusTimeForRun(at)
		}
	}
	return loss, nil
}
