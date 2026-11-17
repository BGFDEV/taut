package staging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/bgfdev/taut/internal/fail"
)

// fsBlobs stores each object as a file under a root directory.
//
// Every file operation goes through an os.Root, which refuses at the system
// call level to resolve any name outside the root, including through ".."
// or a symbolic link planted inside it. Key validation is therefore a first
// line of defence, not the only one.
type fsBlobs struct {
	root *os.Root
}

// NewFS returns a Blobs that keeps objects as files under root, creating the
// directory if needed. Directories are created 0700 and files 0600.
//
// Put writes to a temporary file in the destination directory, fsyncs it and
// renames it into place, which is atomic on POSIX filesystems.
//
// Unlike S3, a filesystem cannot hold both "a" and "a/b"; such a Put fails
// with staging.key_conflict. The key helpers in this package never produce
// that pattern.
func NewFS(root string) (Blobs, error) {
	if root == "" {
		return nil, fail.New("staging.config_invalid",
			"The staging directory is not set",
			"The filesystem staging backend needs a directory to store objects in.",
			"Set the staging directory in the config file, then retry.")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fsError("create the staging directory", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fsError("open the staging directory", err)
	}
	return &fsBlobs{root: r}, nil
}

// Close releases the directory handle.
func (f *fsBlobs) Close() error { return f.root.Close() }

func (f *fsBlobs) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	src := &sourceReader{ctx: ctx, r: r}
	if err := ctx.Err(); err != nil {
		return 0, src.failed(key, err)
	}
	name := filepath.FromSlash(key)
	dir := filepath.Dir(name)

	// A concurrent DeletePrefix may remove the (still empty) directory
	// between MkdirAll and the create; retry a few times.
	var tmp *os.File
	var tmpName string
	for attempt := 0; ; attempt++ {
		if dir != "." {
			if err := f.root.MkdirAll(dir, 0o700); err != nil {
				return 0, f.putError(key, err)
			}
		}
		var suffix [12]byte
		_, _ = rand.Read(suffix[:])
		tmpName = filepath.Join(dir, tmpPrefix+hex.EncodeToString(suffix[:]))
		var err error
		tmp, err = f.root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || attempt == 4 {
			return 0, f.putError(key, err)
		}
	}
	discard := func() {
		_ = tmp.Close()
		_ = f.root.Remove(tmpName)
	}

	n, err := io.Copy(tmp, src)
	if err == nil {
		err = tmp.Sync()
	}
	if err != nil {
		discard()
		if src.err != nil {
			return 0, src.failed(key, src.err)
		}
		return 0, f.putError(key, err)
	}
	if err := tmp.Close(); err != nil {
		_ = f.root.Remove(tmpName)
		return 0, f.putError(key, err)
	}
	if err := ctx.Err(); err != nil {
		_ = f.root.Remove(tmpName)
		return 0, src.failed(key, err)
	}
	if err := f.root.Rename(tmpName, name); err != nil {
		_ = f.root.Remove(tmpName)
		return 0, f.putError(key, err)
	}
	// Make the rename itself durable. Best effort: not every platform can
	// fsync a directory.
	if d, err := f.root.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return n, nil
}

func (f *fsBlobs) putError(key string, err error) error {
	// ENOTDIR: a parent segment is a file. EISDIR/ENOTEMPTY/EEXIST: the key
	// itself is a directory.
	if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return fail.Wrap(err, "staging.key_conflict",
			"A staging object could not be written",
			"The key "+quoteKey(key)+" collides with another object: on a filesystem, one key cannot be both an object and a folder of objects.",
			"This is a bug in Taut. Please report it with the command you ran.")
	}
	return fsError("write a staging object", err)
}

// open returns the object's file, or ErrNotExist if there is no regular file
// under that key.
func (f *fsBlobs) open(key string) (*os.File, fs.FileInfo, error) {
	if err := ValidateKey(key); err != nil {
		return nil, nil, err
	}
	file, err := f.root.Open(filepath.FromSlash(key))
	if err != nil {
		if isAbsent(err) {
			return nil, nil, notExist(key)
		}
		return nil, nil, fsError("read a staging object", err)
	}
	st, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fsError("read a staging object", err)
	}
	if !st.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, notExist(key)
	}
	return file, st, nil
}

func (f *fsBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, _, err := f.open(key)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f *fsBlobs) Stat(ctx context.Context, key string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	file, st, err := f.open(key)
	if err != nil {
		return Info{}, err
	}
	_ = file.Close()
	return Info{Key: key, Size: st.Size()}, nil
}

func (f *fsBlobs) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := filepath.FromSlash(key)
	st, err := f.root.Lstat(name)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return fsError("delete a staging object", err)
	}
	if st.IsDir() {
		return nil // a folder of objects is not an object
	}
	if err := f.root.Remove(name); err != nil && !isAbsent(err) {
		return fsError("delete a staging object", err)
	}
	return nil
}

func (f *fsBlobs) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		if err := validatePrefix(prefix); err != nil {
			yield(Info{}, err)
			return
		}
		f.walk(ctx, prefixDir(prefix), prefix, yield)
	}
}

// prefixDir is the deepest directory that contains every key with the
// prefix ("" for the root).
func prefixDir(prefix string) string {
	if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
		return prefix[:i]
	}
	return ""
}

// readDir returns dir's entries ordered so that a depth-first walk visits
// keys in byte order: a directory sorts as its name followed by a slash.
func (f *fsBlobs) readDir(dir string) ([]fs.DirEntry, error) {
	name := "."
	if dir != "" {
		name = filepath.FromSlash(dir)
	}
	d, err := f.root.Open(name)
	if err != nil {
		if isAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		if isAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	sortName := func(e fs.DirEntry) string {
		if e.IsDir() {
			return e.Name() + "/"
		}
		return e.Name()
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(sortName(a), sortName(b))
	})
	return entries, nil
}

// walk yields the objects under dir whose keys start with prefix. It returns
// false once the consumer or an error has stopped the iteration.
func (f *fsBlobs) walk(ctx context.Context, dir, prefix string, yield func(Info, error) bool) bool {
	if err := ctx.Err(); err != nil {
		yield(Info{}, err)
		return false
	}
	entries, err := f.readDir(dir)
	if err != nil {
		yield(Info{}, fsError("list staging objects", err))
		return false
	}
	for _, e := range entries {
		key := path.Join(dir, e.Name())
		switch {
		case e.IsDir():
			sub := key + "/"
			if strings.HasPrefix(sub, prefix) || strings.HasPrefix(prefix, sub) {
				if !f.walk(ctx, key, prefix, yield) {
					return false
				}
			}
		case !e.Type().IsRegular(), strings.HasPrefix(e.Name(), tmpPrefix), !strings.HasPrefix(key, prefix):
			// Not an object, an upload in progress, or outside the prefix.
		default:
			st, err := e.Info()
			if err != nil {
				if isAbsent(err) {
					continue // deleted while listing
				}
				yield(Info{}, fsError("list staging objects", err))
				return false
			}
			if !yield(Info{Key: key, Size: st.Size()}, nil) {
				return false
			}
		}
	}
	return true
}

func (f *fsBlobs) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	removed := 0
	for info, err := range f.List(ctx, prefix) {
		if err != nil {
			return removed, err
		}
		err := f.root.Remove(filepath.FromSlash(info.Key))
		switch {
		case err == nil:
			removed++
		case isAbsent(err):
		default:
			return removed, fsError("delete staging objects", err)
		}
	}
	dir := prefixDir(prefix)
	f.sweep(dir, prefix)
	if dir != "" && strings.HasSuffix(prefix, "/") {
		_ = f.root.Remove(filepath.FromSlash(dir)) // fails unless empty
	}
	return removed, nil
}

// sweep removes, best effort, what a purge would otherwise leave behind
// under prefix: temporary files of interrupted Puts and empty directories.
func (f *fsBlobs) sweep(dir, prefix string) {
	entries, _ := f.readDir(dir)
	for _, e := range entries {
		key := path.Join(dir, e.Name())
		sub := key + "/"
		switch {
		case e.IsDir() && strings.HasPrefix(sub, prefix):
			f.sweep(key, prefix)
			_ = f.root.Remove(filepath.FromSlash(key)) // fails unless empty
		case e.IsDir() && strings.HasPrefix(prefix, sub):
			f.sweep(key, prefix)
		case strings.HasPrefix(e.Name(), tmpPrefix) && strings.HasPrefix(dir+"/", prefix):
			_ = f.root.Remove(filepath.FromSlash(key))
		}
	}
}

// isAbsent reports whether err means "no object here": the path does not
// exist, or one of its parents is a file.
func isAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func fsError(action string, err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fail.Wrap(err, "staging.access_denied",
			"Taut could not "+action,
			"The operating system denied access: "+err.Error(),
			"Make the staging directory readable and writable by the user Taut runs as, then retry.")
	case errors.Is(err, syscall.ENOSPC):
		return fail.Wrap(err, "staging.no_space",
			"Taut could not "+action,
			"The staging disk is full.",
			"Free up space or point staging at a larger volume, then retry. A Slack export needs about as much staging space as its own size, plus the extracted batches.")
	default:
		return fail.Wrap(err, "staging.fs_error",
			"Taut could not "+action, "",
			"Check that the staging directory exists, is on a healthy disk and is writable, then retry.")
	}
}
