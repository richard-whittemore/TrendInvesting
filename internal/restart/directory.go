package restart

import (
	"bytes"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// Directory is what Classify finds among the logs of one log directory.
type Directory struct {
	// Runs are the runs, in the order the logs were given: least recently
	// modified first.
	Runs []string
	// Abandoned maps each abandoned resume attempt to the run it copied.
	Abandoned map[string]string
	// EmptyStarts are the logs holding no committed record.
	EmptyStarts []string
}

// Latest returns the latest run, the last of Runs, and false when there is
// none, as for a first run.
func (d Directory) Latest() (string, bool) {
	if len(d.Runs) == 0 {
		return "", false
	}
	return d.Runs[len(d.Runs)-1], true
}

// Classify sorts the logs of one directory, names[i] holding recovered[i]
// and the logs given least recently modified first, into runs, abandoned
// attempts and abandoned starts. It leaves out of the runs each log holding
// no committed record, an abandoned start: a run creates its log, header
// first, before it commits its first record, so a fresh run, or a resume,
// that crashed in that window leaves one, and nothing in it tells the two
// apart or is there to resume. It leaves out too each log whose committed
// records are a prefix of an older log's under the same header: a resume of
// that log abandoned partway (ADR 0027 §6: a resume's new log begins with
// the old log's committed part, byte for byte, the configuration first),
// attributed to the newest run it is a prefix of, since a resume of a
// resumed run is a prefix of every run that run continued, and never to
// another attempt, which is no run to resume. A log holding only its
// configuration that is no such prefix is a start cut short (AttemptAtStart,
// CutShort), and stays a run.
func Classify(names []string, recovered []journal.Recovery) Directory {
	found := Directory{Abandoned: map[string]string{}}
	for i, name := range names {
		if len(recovered[i].Entries) == 0 {
			found.EmptyStarts = append(found.EmptyStarts, name)
			continue
		}
		// A log holding a record is never a prefix of an empty one, so an
		// empty start is never the source of an attempt.
		source := ""
		for j := i - 1; j >= 0; j-- {
			// An attempt is never the log another attempt copied: a prefix
			// of an attempt is a prefix of that attempt's own source, which
			// the scan still reaches.
			if _, attempt := found.Abandoned[names[j]]; attempt {
				continue
			}
			if committedPrefixOf(recovered[i], recovered[j]) {
				source = names[j]
				break
			}
		}
		if source != "" {
			found.Abandoned[name] = source
			continue
		}
		found.Runs = append(found.Runs, name)
	}
	return found
}

// committedPrefixOf reports whether attempt's committed entries, however
// few, are a prefix of source's under the same header, compared by kind and
// canonical envelope bytes as a resume compares them (ADR 0027 §6).
func committedPrefixOf(attempt, source journal.Recovery) bool {
	if attempt.Header != source.Header || len(attempt.Entries) > len(source.Entries) {
		return false
	}
	for k, entry := range attempt.Entries {
		other := source.Entries[k]
		if entry.Kind != other.Kind || !bytes.Equal(event.CanonicalEnvelopeBytes(entry.Envelope), event.CanonicalEnvelopeBytes(other.Envelope)) {
			return false
		}
	}
	return true
}

// CutShort reports whether recovery holds nothing past its configuration: a
// run records its configuration, then its approval, one input at a time, so
// such a log decided nothing and records no state, and a fresh run's
// approval of it is checked against the latest earlier run holding more.
func CutShort(recovery journal.Recovery) bool {
	return len(recovery.Entries) <= 1
}

// AttemptAtStart reports whether recovery is a log of the start header and
// configuration describe that holds nothing past it: recorded under the
// same configuration and build, opening with that configuration input, if
// anything. Such a log decided nothing, and nothing in it tells one attempt
// at the start from another, so a fresh run never repeats that start (ADR
// 0027, the restart model).
func AttemptAtStart(recovery journal.Recovery, header journal.LogHeader, configuration event.Envelope) bool {
	if recovery.Header != header || len(recovery.Entries) > 1 {
		return false
	}
	return len(recovery.Entries) == 0 ||
		bytes.Equal(event.CanonicalEnvelopeBytes(recovery.Entries[0].Envelope), event.CanonicalEnvelopeBytes(configuration))
}

// OpensWith reports whether recovery was recorded under header and opens
// with the configuration input configuration: a fresh run with that start,
// crashed before its approval was recorded, would leave a log no resume
// could tell from a resume of that log abandoned after its configuration
// (ADR 0027 §6).
func OpensWith(recovery journal.Recovery, header journal.LogHeader, configuration event.Envelope) bool {
	return recovery.Header == header && len(recovery.Entries) > 0 &&
		bytes.Equal(event.CanonicalEnvelopeBytes(recovery.Entries[0].Envelope), event.CanonicalEnvelopeBytes(configuration))
}
