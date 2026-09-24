package journal_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// failingAfter delivers the first n bytes of r and then fails with err, so a
// read can be made to fail part-way through a line.
type failingAfter struct {
	r   io.Reader
	n   int
	err error
}

func (f *failingAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

// TestReadReportsAMidLineReadFailureAsTheReadFailure pins that a reader
// failing part-way through a line is reported as that failure, not as the
// malformed JSON bufio.Scanner's partial final token would otherwise produce:
// the cause (a cancellation, an I/O error) is what a caller needs.
func TestReadReportsAMidLineReadFailureAsTheReadFailure(t *testing.T) {
	cause := errors.New("the underlying read failed")
	header := []byte(`{"journal_version":1,"configuration_hash":"x"}` + "\n")
	record := []byte(`{"sequence":1,"kind":"input","envelope":{}}` + "\n")
	for name, cut := range map[string]int{
		"inside the header":       10,
		"inside the first record": len(header) + 10,
	} {
		t.Run(name, func(t *testing.T) {
			raw := append(append([]byte{}, header...), record...)
			_, _, err := journal.Read(&failingAfter{r: bytes.NewReader(raw), n: cut, err: cause})
			if !errors.Is(err, cause) {
				t.Fatalf("Read error = %v, want it to wrap the read failure", err)
			}
		})
	}
}
