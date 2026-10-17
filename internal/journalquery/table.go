package journalquery

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table is the answer to a query: named columns and string cells, already
// formatted, so every output form prints the same figures.
type Table struct {
	Columns []string
	Rows    [][]string
}

// WriteText prints the table aligned, with a header row.
func (t Table) WriteText(w io.Writer) error {
	// The table is composed in memory, where a write cannot fail, and handed
	// to w in one call, so a failing w is reported once and never half-written.
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, strings.Join(t.Columns, "\t"))
	for _, row := range t.Rows {
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
	_, err := w.Write(b.Bytes())
	return err
}

// Output forms a table can be written in.
const (
	FormatText = "text"
	FormatCSV  = "csv"
	FormatJSON = "json"
)

// ValidFormat reports whether format names an output form.
func ValidFormat(format string) bool {
	return format == FormatText || format == FormatCSV || format == FormatJSON
}

// Write prints the table in format.
func (t Table) Write(w io.Writer, format string) error {
	switch format {
	case FormatText:
		return t.WriteText(w)
	case FormatCSV:
		return t.writeCSV(w)
	case FormatJSON:
		return t.writeJSON(w)
	}
	return fmt.Errorf("format %q is not %s, %s or %s", format, FormatText, FormatCSV, FormatJSON)
}

func (t Table) writeCSV(w io.Writer) error {
	var b bytes.Buffer
	cw := csv.NewWriter(&b)
	_ = cw.Write(t.Columns) // writes to a bytes.Buffer cannot fail
	_ = cw.WriteAll(t.Rows)
	_, err := w.Write(b.Bytes())
	return err
}

// writeJSON prints an array of objects, one per row, keys in column order and
// every value the same string the text table shows.
func (t Table) writeJSON(w io.Writer) error {
	var b bytes.Buffer
	b.WriteByte('[')
	for r, row := range t.Rows {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for c, cell := range row {
			if c > 0 {
				b.WriteByte(',')
			}
			for _, part := range []string{t.Columns[c], cell} {
				enc, _ := json.Marshal(part) // a string always marshals
				b.Write(enc)
				if part == t.Columns[c] {
					b.WriteByte(':')
				}
			}
		}
		b.WriteByte('}')
	}
	b.WriteString("]\n")
	_, err := w.Write(b.Bytes())
	return err
}
