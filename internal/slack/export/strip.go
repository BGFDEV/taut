package export

import (
	"archive/zip"
	"io"
	"path"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
)

// HasDirectMessages reports whether the export ZIP at zipPath carries
// direct messages or group direct messages: their index files, whatever
// they list.
func HasDirectMessages(zipPath string) (bool, error) {
	e, err := Open(zipPath)
	if err != nil {
		return false, err
	}
	defer e.Close()
	return e.HasFile(dmsFile) || e.HasFile(mpimsFile), nil
}

// StripDirectMessages writes a copy of the export ZIP at zipPath to w
// without its direct messages: dms.json, mpims.json, and every folder that is
// not a channel's. What stays is copied as it is, compressed bytes
// included. It returns how many entries were left out.
func StripDirectMessages(zipPath string, w io.Writer) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, fail.Wrap(err, "export.not_zip", zipPath+" is not a readable ZIP archive", "",
			"Download the export from Slack again and check the file size matches.")
	}
	defer zr.Close()
	root, ok := locateRoot(&zr.Reader)
	if !ok {
		return 0, fail.New("export.not_slack_export", zipPath+" does not look like a Slack export", "", nextReexport)
	}
	e, err := OpenFS(&zr.Reader, zipPath)
	if err != nil {
		return 0, err
	}
	chans, err := e.Channels()
	if err != nil {
		return 0, err
	}
	// What stays is what is known to belong to a channel, not what is not
	// known to belong to a direct message: a folder no index lists (a
	// conversation Slack left out of dms.json, say) goes as well. Nothing
	// reads such a folder; an import uses the index files and the folders
	// of the channels they list.
	keep := map[string]bool{}
	for _, ch := range chans {
		if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM {
			continue
		}
		if folder, err := channelFolder(ch); err == nil {
			keep[folder] = true
		}
	}
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}
	private := func(name string, dir bool) bool {
		// As the archive's file system sees the name, so that no second
		// spelling of a path gets past.
		name = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, `\`, "/")), "/")
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok {
			// Outside the export proper: what an archive tool added
			// beside it, which can mirror any folder's names.
			return name+"/" != prefix
		}
		folder, _, nested := strings.Cut(rel, "/")
		if !nested && !dir {
			// A file at the top: an index file, or something Slack put
			// beside them.
			return rel == dmsFile || rel == mpimsFile
		}
		// A folder's own entry goes with the folder: the name of a group
		// conversation's folder is made of its participants' names.
		return !keep[folder]
	}
	zw := zip.NewWriter(w)
	removed := 0
	for _, f := range zr.File {
		if private(f.Name, f.FileInfo().IsDir()) {
			removed++
			continue
		}
		if err := zw.Copy(f); err != nil {
			return removed, fail.Wrap(err, "export.unreadable", "The Slack export could not be copied", "",
				"Check the file and the free space in the temporary folder, then retry.")
		}
	}
	if err := zw.Close(); err != nil {
		return removed, fail.Wrap(err, "export.unreadable", "The Slack export could not be copied", "",
			"Check the free space in the temporary folder, then retry.")
	}
	return removed, nil
}
