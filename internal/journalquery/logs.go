package journalquery

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// ListLogs returns the durable logs in dir, the regular files named with
// suffix, in restart.OlderLog order. A log that vanishes while it is listed
// is skipped; one that cannot be examined for any other reason refuses, so
// the latest run is never passed over silently.
func ListLogs(dir, suffix string) ([]restart.LoggedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list the durable log's directory %s: %w", dir, err)
	}
	return listed(dir, suffix, entries)
}

// listed is ListLogs of the entries read from dir.
func listed(dir, suffix string, entries []fs.DirEntry) ([]restart.LoggedFile, error) {
	var logs []restart.LoggedFile
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("examine %s in %s: %w", entry.Name(), dir, err)
		}
		logs = append(logs, restart.LoggedFile{Name: entry.Name(), Modified: info.ModTime()})
	}
	sort.Slice(logs, func(i, j int) bool { return restart.OlderLog(logs[i], logs[j]) })
	return logs, nil
}
