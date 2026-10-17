package restart

import (
	"errors"
	"io/fs"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// ReadLog is one log of a directory as an attempt to read it left it: what
// it holds, or the error that kept it from being read.
type ReadLog struct {
	Name     string
	Modified time.Time
	Recovery journal.Recovery
	Err      error
}

// ReadFault is which of the three cases a log that could not be opened or
// read is, each with its own operator step.
type ReadFault int

const (
	// PermissionFault: the engine is not permitted to read the log. Its
	// owner or mode is fixed; it is never set aside.
	PermissionFault ReadFault = iota + 1
	// MayBeLatestFault: a fault such as the disk, on a log no log holding a
	// record was modified after, so it is or may be the latest run, an
	// intact crashed run, never set aside while the disk may yet be fixed.
	MayBeLatestFault
	// SupersededFault: a fault such as the disk, on a log a later log
	// holding a record was modified after. It may be set aside, unchanged
	// (ADR 0018), once the operator rules out that the later log is a resume
	// of it abandoned partway.
	SupersededFault
)

// ClassifyReadFault classifies logs[i], which could not be opened or read,
// among logs ordered least recently modified first: a fault in reading it,
// not damage to what it holds, so it may be an intact crashed run. For a
// SupersededFault it also returns the latest log holding a committed record
// that was modified after it.
func ClassifyReadFault(logs []ReadLog, i int) (kind ReadFault, newer string) {
	if errors.Is(logs[i].Err, fs.ErrPermission) {
		return PermissionFault, ""
	}
	for j := i + 1; j < len(logs); j++ {
		if logs[j].Err == nil && len(logs[j].Recovery.Entries) > 0 && logs[j].Modified.After(logs[i].Modified) {
			newer = logs[j].Name
		}
	}
	if newer == "" {
		return MayBeLatestFault, ""
	}
	return SupersededFault, newer
}
