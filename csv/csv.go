package csv

import (
	"encoding/csv"
	"io"

	"golang.org/x/xerrors"
)

// Produces a list of fields making up a record.
type Recorder interface {
	Record() []string
}

// Optionally produces a list of column names describing the fields of a
// record. Values that also implement HeaderRecorder have a header row
// written before their first record.
type HeaderRecorder interface {
	Headers() []string
}

// An Encoder writes CSV records to an output stream.
type Encoder struct {
	w           *csv.Writer
	wroteHeader bool
}

// NewEncoder returns a new encoder that writes to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: csv.NewWriter(w)}
}

// Encode writes a CSV record representing v to the stream followed by a
// newline character. Value given must implement the Recorder interface. If
// the value implements HeaderRecorder, a header row is written before the
// first record.
func (enc *Encoder) Encode(v interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = xerrors.Errorf("recovered: %w", e)
			} else {
				err = xerrors.Errorf("recovered: %v", r)
			}
		}
	}()

	if !enc.wroteHeader {
		enc.wroteHeader = true
		if hr, ok := v.(HeaderRecorder); ok {
			if headers := hr.Headers(); len(headers) > 0 {
				enc.w.Write(headers)
			}
		}
	}

	enc.w.Write(v.(Recorder).Record())
	enc.w.Flush()

	return enc.w.Error()
}
