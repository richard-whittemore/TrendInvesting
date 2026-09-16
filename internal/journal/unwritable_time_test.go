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

func TestWriteReturnsTimestampEncodingErrors(t *testing.T) {
	for _, field := range []string{"SpanStart", "SpanEnd", "EventTime", "RecordedAt"} {
		for _, kind := range []string{"year", "offset"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				header := testHeader()
				entries := testEntries(1)
				bad := time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC)
				if kind == "offset" {
					bad = time.Date(2026, 3, 2, 0, 0, 0, 0, time.FixedZone("out-of-range", 24*60*60))
				}
				switch field {
				case "SpanStart":
					header.SpanStart, header.SpanEnd = bad, bad
				case "SpanEnd":
					header.SpanEnd = bad
				case "EventTime":
					entries[0].Envelope.EventTime = bad
				case "RecordedAt":
					entries[0].Envelope.RecordedAt = bad
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
