package journal_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestWriteReturnsTimestampEncodingErrors exercises the encoding-error path
// for every timestamp a journal writes. The validated-payload-json invariant
// (docs/development.md) covers payloads, not envelopes or headers, so a
// timestamp encoding/json refuses reaches the encoder here.
//
// A header whose span its own inputs deny is refused before anything is
// written, so each case below states a span the records agree with: an
// unwritable span is carried by the inputs too, and an unwritable record time
// is put on a decision, whose event time the span does not speak for.
func TestWriteReturnsTimestampEncodingErrors(t *testing.T) {
	for _, field := range []string{"SpanStart", "SpanEnd", "EventTime", "RecordedAt"} {
		for _, kind := range []string{"year", "offset"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				bad := time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC)
				if kind == "offset" {
					bad = time.Date(2026, 3, 2, 0, 0, 0, 0, time.FixedZone("out-of-range", 24*60*60))
				}
				// testEntries(3) records an input, a decision and an input:
				// entries 0 and 2 are what the span speaks for.
				header, entries := testHeader(), testEntries(3)
				switch field {
				case "SpanStart":
					header.SpanStart, header.SpanEnd = bad, bad
					entries[0].Envelope.EventTime, entries[2].Envelope.EventTime = bad, bad
				case "SpanEnd":
					header.SpanEnd = bad
					entries[2].Envelope.EventTime = bad
				case "EventTime":
					entries[1].Envelope.EventTime = bad
				case "RecordedAt":
					entries[1].Envelope.RecordedAt = bad
				}
				var out bytes.Buffer
				err := journal.Write(&out, header, entries)
				var marshalErr *json.MarshalerError
				if !errors.As(err, &marshalErr) || !strings.Contains(err.Error(), "journal: encode line:") {
					t.Fatalf("want timestamp encoding error, got %v", err)
				}
			})
		}
	}
}
