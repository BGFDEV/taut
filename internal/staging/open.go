package staging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
)

// Config selects and configures a staging backend.
type Config struct {
	// Backend is "s3" or "fs".
	Backend string
	// FSRoot is the directory for the "fs" backend.
	FSRoot string
	// S3 configures the "s3" backend.
	S3 S3Config
}

// Open returns the backend cfg selects. The result does NOT encrypt: wrap it
// with Encrypted before storing anything.
func Open(cfg Config) (Blobs, error) {
	switch cfg.Backend {
	case "fs":
		return NewFS(cfg.FSRoot)
	case "s3":
		return NewS3(cfg.S3)
	default:
		return nil, fail.New("staging.config_invalid",
			"The staging backend is not valid",
			"The backend must be \"s3\" or \"fs\".",
			"Set the staging backend in the config file to s3 or fs, then retry.")
	}
}

// SHA256Reader returns a reader that hashes everything read through it, and
// a function giving the hex SHA-256 of what has passed so far. Wrap the
// plaintext handed to Put to record its digest while it uploads.
func SHA256Reader(r io.Reader) (io.Reader, func() string) {
	h := sha256.New()
	return io.TeeReader(r, h), func() string { return hex.EncodeToString(h.Sum(nil)) }
}

// RunPrefix returns the key prefix that holds everything staged for a run:
// "runs/<id>/". Purging the run is DeletePrefix(RunPrefix(id)).
func RunPrefix(runID string) string {
	return "runs/" + safeSegment(runID) + "/"
}

// ExportKey returns the key of a run's uploaded Slack export.
func ExportKey(runID string) string {
	return RunPrefix(runID) + "export/slack-export.zip"
}

// BatchKey returns the key of one batch file, for example
// "runs/<id>/extract/<laneKey>/000042.ndjson.gz". Batches of a lane list in
// seq order.
//
// Key names are visible to the object store. runID, stage and laneKey must
// be identifiers (a channel ID, a date), never names or message content.
func BatchKey(runID, stage, laneKey string, seq int) string {
	return fmt.Sprintf("%s%s/%s/%06d.ndjson.gz", RunPrefix(runID), safeSegment(stage), safeSegment(laneKey), max(seq, 0))
}

// safeSegment maps any string to a single, valid, non-empty path segment.
// Letters, digits, '.', '_' and '-' are kept; every other byte becomes
// "~XX" (uppercase hex), as does a leading '.', so the result can never be
// ".", ".." or a reserved temporary name. The mapping is injective, so
// distinct inputs never share a key. The empty string maps to "~", and an
// input too long for a file name is cut and completed with a hash of the
// whole input.
func safeSegment(s string) string {
	const maxLen = 120
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_', c == '-',
			c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "~%02X", c)
		}
	}
	out := b.String()
	switch {
	case out == "":
		return "~"
	case len(out) > maxLen:
		sum := sha256.Sum256([]byte(s))
		return out[:maxLen-34] + "~h" + hex.EncodeToString(sum[:16])
	}
	return out
}
