// Package export reads a Slack workspace export, either the .zip Slack
// produces or a directory it was extracted into.
//
// The archive is never loaded into memory: a ZIP is read through its central
// directory with random access, and channel history is decoded one day file
// at a time.
package export

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
)

// Index files at the root of an export.
const (
	usersFile    = "users.json"
	orgUsersFile = "org_users.json" // Enterprise Grid
	channelsFile = "channels.json"
	groupsFile   = "groups.json"
	mpimsFile    = "mpims.json"
	dmsFile      = "dms.json"
)

// channelIndexes lists the channel index files in the order Channels reports
// them.
var channelIndexes = []struct {
	file string
	kind slack.ChannelKind
}{
	{channelsFile, slack.KindPublic},
	{groupsFile, slack.KindPrivate},
	{mpimsFile, slack.KindMPIM},
	{dmsFile, slack.KindDM},
}

const nextReexport = "Export the workspace again from Slack (Admin > Workspace settings > Security > Import & export data) and pass the downloaded .zip to Taut without unpacking or editing it."

// Export is an open Slack export. Its methods are safe for concurrent use;
// Close must not be called while reads are in flight.
type Export struct {
	fsys   fs.FS // rooted at the folder holding the index files
	label  string
	closer io.Closer
}

// Open opens the Slack export at path, which may be a .zip file or a
// directory holding an extracted export. A single top-level folder wrapping
// the export (as archive tools often add) is tolerated in both forms.
//
// Errors are *fail.Error values with codes export.not_found,
// export.unreadable, export.not_zip or export.not_slack_export.
func Open(path string) (*Export, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fail.Wrap(err, "export.not_found",
				fmt.Sprintf("There is no Slack export at %s", path),
				"No file or folder exists at that path.",
				"Check the path for typos. It should point at the .zip Slack sent you, or at the folder you extracted it into.")
		}
		return nil, unreadable(path, err)
	}

	if info.IsDir() {
		root, err := os.OpenRoot(path)
		if err != nil {
			return nil, unreadable(path, err)
		}
		e, err := OpenFS(root.FS(), path)
		if err != nil {
			root.Close()
			return nil, err
		}
		e.closer = root
		return e, nil
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, unreadable(path, err)
		}
		return nil, fail.Wrap(err, "export.not_zip",
			fmt.Sprintf("%s is not a readable ZIP archive", path),
			"The file is not a ZIP, or its download was cut short and the archive is incomplete.",
			"Download the export from Slack again and check the file size matches. If you already extracted it, pass the extracted folder instead.")
	}
	e, err := OpenFS(&zr.Reader, path)
	if err != nil {
		zr.Close()
		return nil, err
	}
	e.closer = zr
	return e, nil
}

// OpenFS opens an export held in an arbitrary file system. label names the
// source in error messages. The caller keeps ownership of fsys; Close on the
// returned Export is a no-op.
func OpenFS(fsys fs.FS, label string) (*Export, error) {
	root, ok := locateRoot(fsys)
	if !ok {
		return nil, fail.New("export.not_slack_export",
			fmt.Sprintf("%s does not look like a Slack export", label),
			"It contains none of the files every Slack export has (users.json, channels.json, groups.json, dms.json, mpims.json), neither at the top level nor inside a single wrapping folder.",
			nextReexport)
	}
	if root != "." {
		sub, err := fs.Sub(fsys, root)
		if err != nil {
			return nil, unreadable(label, err)
		}
		fsys = sub
	}
	return &Export{fsys: fsys, label: label}, nil
}

// locateRoot finds the directory holding the export's index files: the top
// level, or the only top-level folder when the export was wrapped in one.
func locateRoot(fsys fs.FS) (string, bool) {
	if isExportRoot(fsys, ".") {
		return ".", true
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return "", false
	}
	var dirs []string
	for _, entry := range entries {
		name := entry.Name()
		// Skip archive-tool litter such as __MACOSX and dot folders.
		if !entry.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "__") {
			continue
		}
		dirs = append(dirs, name)
	}
	if len(dirs) == 1 && isExportRoot(fsys, dirs[0]) {
		return dirs[0], true
	}
	return "", false
}

func isExportRoot(fsys fs.FS, dir string) bool {
	for _, name := range []string{usersFile, orgUsersFile, channelsFile, groupsFile, dmsFile, mpimsFile} {
		if info, err := fs.Stat(fsys, path.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// Close releases the underlying archive or directory handle. It is safe to
// call more than once.
func (e *Export) Close() error {
	if e.closer == nil {
		return nil
	}
	err := e.closer.Close()
	e.closer = nil
	return err
}

// FS returns the export's file system, rooted at the folder that holds the
// index files. It gives access to files this package does not model, such as
// canvases.json and integration_logs.json.
func (e *Export) FS() fs.FS { return e.fsys }

// HasFile reports whether the export contains a regular file with the given
// slash-separated name relative to the export root, such as "canvases.json".
func (e *Export) HasFile(name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	info, err := fs.Stat(e.fsys, name)
	return err == nil && !info.IsDir()
}

// Users returns the workspace's users from users.json and, on Enterprise
// Grid, org_users.json. When both exist they are merged by ID with users.json
// taking precedence.
//
// It returns a *fail.Error with code export.users_missing if neither file
// exists, and export.corrupt_index if one cannot be decoded.
func (e *Export) Users() ([]slack.User, error) {
	var (
		users []slack.User
		seen  = map[string]bool{}
		found bool
	)
	for _, name := range []string{usersFile, orgUsersFile} {
		if !e.HasFile(name) {
			continue
		}
		found = true
		err := e.readArray(name, func(i int, raw json.RawMessage) error {
			var u slack.User
			if err := unmarshalObject(raw, &u); err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			if u.ID != "" && seen[u.ID] {
				return nil
			}
			seen[u.ID] = true
			users = append(users, u)
			return nil
		})
		if err != nil {
			return nil, e.corruptIndex(name, err)
		}
	}
	if !found {
		return nil, fail.New("export.users_missing",
			fmt.Sprintf("The export at %s has no user list", e.label),
			"Neither users.json nor org_users.json is present. Taut needs the user list to map Slack authors to Microsoft 365 accounts.",
			nextReexport)
	}
	return users, nil
}

// Channels returns every conversation in the export: public channels
// (channels.json), private channels (groups.json), group DMs (mpims.json) and
// DMs (dms.json), in that order and in file order within each. Index files
// that do not exist are skipped, so an export of public channels only yields
// public channels only. Each Channel has its Kind set.
//
// It returns a *fail.Error with code export.corrupt_index if an index file
// cannot be decoded.
func (e *Export) Channels() ([]slack.Channel, error) {
	var channels []slack.Channel
	for _, idx := range channelIndexes {
		if !e.HasFile(idx.file) {
			continue
		}
		err := e.readArray(idx.file, func(i int, raw json.RawMessage) error {
			var c slack.Channel
			if err := unmarshalObject(raw, &c); err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			c.Kind = idx.kind
			channels = append(channels, c)
			return nil
		})
		if err != nil {
			return nil, e.corruptIndex(idx.file, err)
		}
	}
	return channels, nil
}

// Days returns the sorted YYYY-MM-DD keys of the day files present for ch.
// A channel with no folder in the export (it never had a message) yields an
// empty list and no error.
func (e *Export) Days(ch slack.Channel) ([]string, error) {
	folder, err := channelFolder(ch)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(e.fsys, folder)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, unreadable(path.Join(e.label, folder), err)
	}
	var days []string
	for _, entry := range entries {
		day, isJSON := strings.CutSuffix(entry.Name(), ".json")
		if isJSON && !entry.IsDir() && isDayKey(day) {
			days = append(days, day)
		}
	}
	slices.Sort(days)
	return days, nil
}

// channelFolder returns the export folder for ch, refusing anything that
// could address a path outside the channel's own folder.
func channelFolder(ch slack.Channel) (string, error) {
	folder := ch.Folder()
	if folder == "" || folder == "." || strings.ContainsAny(folder, `/\`) || !fs.ValidPath(folder) {
		return "", fail.New("export.bad_channel",
			fmt.Sprintf("Channel %q cannot be located in the export", ch.Label()),
			"Its name is empty or is not a valid folder name, so its day files cannot be addressed.",
			"Check the channel's entry in channels.json, groups.json, dms.json or mpims.json; every entry needs an id and (except for DMs) a name.")
	}
	return folder, nil
}

// isDayKey reports whether s has the shape YYYY-MM-DD.
func isDayKey(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 4 || i == 7 {
			if s[i] != '-' {
				return false
			}
		} else if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// readArray streams the top-level JSON array in the named file, calling each
// with one raw element at a time. The raw slice is reused between calls.
func (e *Export) readArray(name string, each func(i int, raw json.RawMessage) error) error {
	f, err := e.fsys.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReaderSize(f, 64<<10))
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the file is empty")
		}
		return err
	}
	if tok != json.Delim('[') {
		return errors.New("expected a JSON array at the top level")
	}
	var raw json.RawMessage
	for i := 0; dec.More(); i++ {
		raw = raw[:0]
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if err := each(i, raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("unterminated array: %w", err)
	}
	return nil
}

// unmarshalObject decodes raw into v, insisting that raw is a JSON object.
// encoding/json would otherwise accept null silently.
func unmarshalObject(raw json.RawMessage, v any) error {
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("entry is not a JSON object")
	}
	return json.Unmarshal(raw, v)
}

func unreadable(what string, err error) *fail.Error {
	return fail.Wrap(err, "export.unreadable",
		fmt.Sprintf("Taut cannot read %s", what), "",
		"Check that the path exists and that your user has permission to read it, then run the command again.")
}

func (e *Export) corruptIndex(name string, err error) *fail.Error {
	return fail.Wrap(err, "export.corrupt_index",
		fmt.Sprintf("%s in the export at %s is damaged", name, e.label), "",
		nextReexport)
}
