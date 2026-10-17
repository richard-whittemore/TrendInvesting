package restart_test

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// TestAReadFaultIsToldApartFromTheLatestRun: a log that could not be opened
// or read may be an intact crashed run. A permission fault is fixed at the
// file; a disk fault on a log no log holding a record was modified after is
// or may be the latest run; one with such a later log is superseded, the
// latest such log named (ADR 0018: evidence is set aside, never deleted).
func TestAReadFaultIsToldApartFromTheLatestRun(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, time.October, 17, 13, minute, 0, 0, time.UTC) }
	disk := errors.New("input/output error")
	held := logOf(header, configuration)
	for _, c := range []struct {
		name  string
		logs  []restart.ReadLog
		kind  restart.ReadFault
		newer string
	}{
		{"permission, superseded or not", []restart.ReadLog{
			{Name: "a.log", Modified: at(1), Err: fmt.Errorf("open: %w", fs.ErrPermission)},
			{Name: "b.log", Modified: at(2), Recovery: held},
		}, restart.PermissionFault, ""},
		{"disk, the last log", []restart.ReadLog{
			{Name: "a.log", Modified: at(1), Recovery: held},
			{Name: "b.log", Modified: at(2), Err: disk},
		}, restart.MayBeLatestFault, ""},
		{"disk, later logs hold nothing, fail, or share its time", []restart.ReadLog{
			{Name: "a.log", Modified: at(1), Err: disk},
			{Name: "b.log", Modified: at(2), Recovery: logOf(header)},
			{Name: "c.log", Modified: at(3), Err: disk},
			{Name: "d.log", Modified: at(1), Recovery: held},
		}, restart.MayBeLatestFault, ""},
		{"disk, superseded", []restart.ReadLog{
			{Name: "a.log", Modified: at(1), Err: disk},
			{Name: "b.log", Modified: at(2), Recovery: held},
			{Name: "c.log", Modified: at(3), Recovery: held},
			{Name: "d.log", Modified: at(4), Recovery: logOf(header)},
		}, restart.SupersededFault, "c.log"},
	} {
		i := 0
		for j, log := range c.logs {
			if log.Err != nil {
				i = j
				break
			}
		}
		kind, newer := restart.ClassifyReadFault(c.logs, i)
		if kind != c.kind || newer != c.newer {
			t.Errorf("%s: ClassifyReadFault = %v, %q, want %v, %q", c.name, kind, newer, c.kind, c.newer)
		}
	}
}
