package auditlog

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
)

// Format is an export file format.
type Format string

// The export formats.
const (
	NDJSON Format = "ndjson" // one JSON object per line
	CSV    Format = "csv"    // RFC 4180 with a header row
)

// ParseFormat parses a --format value.
func ParseFormat(s string) (Format, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ndjson", "jsonl":
		return NDJSON, true
	case "csv":
		return CSV, true
	}
	return "", false
}

// FormatFor infers the format from a file name: .csv is CSV, anything else
// is NDJSON.
func FormatFor(path string) Format {
	if strings.EqualFold(filepath.Ext(path), ".csv") {
		return CSV
	}
	return NDJSON
}

// CodeExists is the error code Export returns when the file is already
// there and overwriting was not allowed.
const CodeExists = "audit.export_exists"

// ExportOptions say where and how to export.
type ExportOptions struct {
	Path   string
	Format Format // empty: inferred from Path
	// Limit stops after this many entries; zero exports every match.
	Limit int
	// Overwrite allows replacing an existing file.
	Overwrite bool
}

// ExportResult describes a finished export.
type ExportResult struct {
	Path    string `json:"path"`
	Format  Format `json:"format"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
	// SHA256 is the hex digest of the file as written, so the export can
	// itself be shown to be unaltered later.
	SHA256 string `json:"sha256"`
	// Truncated is true when a limit stopped the export before the last
	// matching entry.
	Truncated bool `json:"truncated"`
}

// Export writes every entry matching f to a file.
//
// The file appears under its final name only once it is complete: entries
// are written to a temporary file in the same directory, flushed to disk and
// then moved into place, so a reader never sees half an export and a failure
// leaves nothing behind. The file is private to the user (0600).
func Export(ctx context.Context, src Source, f store.LogFilter, o ExportOptions) (ExportResult, error) {
	res := ExportResult{Path: o.Path, Format: o.Format}
	if res.Format == "" {
		res.Format = FormatFor(o.Path)
	}
	if strings.TrimSpace(o.Path) == "" {
		return res, fail.Usagef("Pass a file name, such as `--export audit.ndjson`.", "No export file was named")
	}

	switch info, err := os.Lstat(o.Path); {
	case err == nil && info.IsDir():
		return res, fail.New("audit.export_is_dir",
			"Cannot export to "+o.Path,
			"It is a directory.",
			"Name a file inside it, such as `"+filepath.Join(o.Path, "audit."+string(res.Format))+"`.").WithExit(fail.ExitUsage)
	case err == nil && !o.Overwrite:
		return res, exists(o.Path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return res, exportFailed(o.Path, err)
	}

	dir := filepath.Dir(o.Path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(o.Path)+".*.tmp")
	if err != nil {
		return res, exportFailed(o.Path, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return res, exportFailed(o.Path, err)
	}

	sum := sha256.New()
	count := &counter{}
	buf := bufio.NewWriterSize(io.MultiWriter(tmp, sum, count), 1<<16)
	enc := NewEncoder(buf, res.Format)
	if err := enc.Begin(); err != nil {
		return res, exportFailed(o.Path, err)
	}
	more, err := Walk(ctx, src, f, o.Limit, func(e store.LogEntry) error {
		if err := enc.Entry(e); err != nil {
			return exportFailed(o.Path, err)
		}
		res.Entries++
		return nil
	})
	if err != nil {
		// Reading failed, or writing did: either way nothing is left
		// behind under the final name.
		return res, err
	}
	res.Truncated = more
	if err := enc.End(); err != nil {
		return res, exportFailed(o.Path, err)
	}
	if err := buf.Flush(); err != nil {
		return res, exportFailed(o.Path, err)
	}
	if err := tmp.Sync(); err != nil {
		return res, exportFailed(o.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return res, exportFailed(o.Path, err)
	}
	if err := commit(tmp.Name(), o.Path, o.Overwrite); err != nil {
		return res, err
	}
	committed = true
	res.Bytes = count.n
	res.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return res, nil
}

// commit moves the finished temporary file to its final name. Without
// overwrite it links rather than renames, because a link fails if the name
// was taken while the export ran, where a rename would silently replace it.
func commit(tmp, path string, overwrite bool) error {
	if overwrite {
		if err := os.Rename(tmp, path); err != nil {
			return exportFailed(path, err)
		}
		return nil
	}
	err := os.Link(tmp, path)
	switch {
	case err == nil:
		_ = os.Remove(tmp)
		return nil
	case errors.Is(err, fs.ErrExist):
		return exists(path)
	}
	// The filesystem has no hard links. Fall back to checking, then moving.
	if _, statErr := os.Lstat(path); statErr == nil {
		return exists(path)
	}
	if err := os.Rename(tmp, path); err != nil {
		return exportFailed(path, err)
	}
	return nil
}

func exists(path string) error {
	return fail.New(CodeExists,
		path+" already exists",
		"An export is evidence, so Taut does not replace one unless asked.",
		"Choose another file name, or add `--yes` to replace it.").WithExit(fail.ExitUsage)
}

func exportFailed(path string, err error) error {
	why := err.Error()
	var pe *fs.PathError
	if errors.As(err, &pe) {
		why = pe.Err.Error() + ": " + pe.Path
	}
	return fail.Wrap(err, "audit.export_failed",
		"Could not write the export to "+path, why,
		"Check that the directory exists and is writable, then run the export again.")
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// Encoder writes entries in one export format.
type Encoder struct {
	format Format
	json   *json.Encoder
	csv    *csv.Writer
}

// NewEncoder returns an encoder writing format to w.
func NewEncoder(w io.Writer, format Format) *Encoder {
	e := &Encoder{format: format}
	if format == CSV {
		e.csv = csv.NewWriter(w)
	} else {
		e.json = json.NewEncoder(w)
		e.json.SetEscapeHTML(false)
	}
	return e
}

// CSVHeader is the header row of a CSV export. The names are the JSON field
// names, so both formats describe an entry in the same words.
var CSVHeader = []string{
	"stream", "seq", "at", "actor", "op", "sourceId", "targetId",
	"contentHash", "outcome", "detail", "prevHash", "entryHash",
}

// Begin writes whatever precedes the first entry.
func (e *Encoder) Begin() error {
	if e.csv != nil {
		return e.csv.Write(CSVHeader)
	}
	return nil
}

// Entry writes one entry. Times are written in UTC.
func (e *Encoder) Entry(le store.LogEntry) error {
	le.At = le.At.UTC()
	if e.csv == nil {
		return e.json.Encode(le)
	}
	return e.csv.Write([]string{
		Cell(le.Stream), strconv.FormatInt(le.Seq, 10), le.At.Format(time.RFC3339Nano),
		Cell(le.Actor), Cell(le.Op), Cell(le.SourceID), Cell(le.TargetID),
		Cell(le.ContentHash), Cell(le.Outcome), Cell(le.Detail), Cell(le.PrevHash), Cell(le.EntryHash),
	})
}

// End flushes anything buffered by the encoder.
func (e *Encoder) End() error {
	if e.csv != nil {
		e.csv.Flush()
		return e.csv.Error()
	}
	return nil
}

// Cell guards a CSV cell against formula injection. A spreadsheet treats a
// cell beginning with =, +, - or @ (or a tab or carriage return, which some
// strip before looking) as a formula; a leading single quote makes it text.
func Cell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
