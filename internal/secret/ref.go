package secret

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
)

// maxFileSize bounds how much of a secret file is read. Tokens, client
// secrets and PEM bundles are all far smaller.
const maxFileSize = 1 << 20

// Warning codes returned by ResolveWithWarnings.
const (
	// WarnFilePermissions means a secret file is readable by its group or
	// by everyone.
	WarnFilePermissions = "secret.file_permissions"
)

// Ref says where a secret lives. Exactly one of Env or File must be set. A
// Ref is safe to store in the config file and to print; it never contains the
// secret itself.
type Ref struct {
	// Env names an environment variable holding the secret.
	Env string `yaml:"env,omitempty" json:"env,omitempty"`
	// File is the path of a file holding the secret. A relative path is
	// resolved against the base directory given to Resolve.
	File string `yaml:"file,omitempty" json:"file,omitempty"`
}

// Warning is a non-fatal finding about how a secret is stored.
type Warning struct {
	// Code is a stable identifier such as WarnFilePermissions.
	Code string `json:"code"`
	// Message says what is wrong.
	Message string `json:"message"`
	// Next is the step that resolves it.
	Next string `json:"next,omitempty"`
}

// String renders the warning as one line.
func (w Warning) String() string {
	if w.Next == "" {
		return w.Message
	}
	return w.Message + " " + w.Next
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsZero reports whether neither Env nor File is set.
func (r Ref) IsZero() bool { return r.Env == "" && r.File == "" }

// Validate checks the shape of the reference without touching the
// environment or the filesystem. A non-nil result is a *fail.Error.
func (r Ref) Validate() error {
	const next = "Reference the secret as `{ env: NAME }` or `{ file: /path/to/secret }`."
	switch {
	case r.Env == "" && r.File == "":
		return fail.New("secret.ref_invalid", "The secret reference is empty",
			"neither `env` nor `file` is set", next).WithExit(fail.ExitUsage)
	case r.Env != "" && r.File != "":
		return fail.New("secret.ref_invalid", "The secret reference is ambiguous",
			"both `env` and `file` are set; exactly one is allowed",
			"Remove either `env` or `file` from the reference.").WithExit(fail.ExitUsage)
	}
	if kind, ok := LooksLikeSecret(r.Env + r.File); ok {
		return fail.New("secret.ref_literal", "The secret reference contains the secret itself",
			fmt.Sprintf("the value looks like a literal %s, not the name of an environment variable or a file path", kind),
			"Export the secret as an environment variable (or write it to a file) and put only that name or path in the reference.").
			WithExit(fail.ExitUsage)
	}
	if r.Env != "" && !envNameRE.MatchString(r.Env) {
		return fail.New("secret.ref_invalid", "The secret reference names an invalid environment variable",
			fmt.Sprintf("%q is not a valid variable name; use letters, digits and underscores, not starting with a digit", r.Env),
			"Use a name such as TAUT_SLACK_TOKEN.").WithExit(fail.ExitUsage)
	}
	if strings.ContainsRune(r.File, 0) {
		return fail.New("secret.ref_invalid", "The secret reference names an invalid file path",
			"the path contains a NUL byte", next).WithExit(fail.ExitUsage)
	}
	return nil
}

// Describe returns a short description of the reference, such as
// "env TAUT_SLACK_TOKEN". It never reads the secret and is safe to print.
func (r Ref) Describe() string {
	clean := func(s string) string {
		if _, ok := LooksLikeSecret(s); ok {
			return Marker
		}
		return Redact(s)
	}
	switch {
	case r.Env != "" && r.File != "":
		return "env " + clean(r.Env) + " and file " + clean(r.File) + " (ambiguous)"
	case r.Env != "":
		return "env " + clean(r.Env)
	case r.File != "":
		return "file " + clean(r.File)
	default:
		return "unset"
	}
}

// Resolve reads the secret. Relative file paths are resolved against
// baseDir; an empty baseDir means the working directory. Warnings are
// discarded; use ResolveWithWarnings to see them. A non-nil error is a
// *fail.Error.
func (r Ref) Resolve(baseDir string) (Value, error) {
	v, _, err := r.ResolveWithWarnings(baseDir)
	return v, err
}

// ResolveWithWarnings is Resolve, additionally returning non-fatal findings
// such as a secret file that other users can read.
func (r Ref) ResolveWithWarnings(baseDir string) (Value, []Warning, error) {
	if err := r.Validate(); err != nil {
		return Value{}, nil, err
	}
	if r.Env != "" {
		v, err := r.resolveEnv()
		return v, nil, err
	}
	return r.resolveFile(baseDir)
}

func (r Ref) resolveEnv() (Value, error) {
	raw, ok := os.LookupEnv(r.Env)
	if !ok {
		return Value{}, fail.New("secret.env_unset",
			fmt.Sprintf("The environment variable %s is not set", r.Env),
			"the config file references it as the source of a secret",
			fmt.Sprintf("Export it in this shell (`export %s=…`) or point the config file at a file with `{ file: /path }`.", r.Env))
	}
	s := trimSecret(raw)
	if s == "" {
		return Value{}, fail.New("secret.empty",
			fmt.Sprintf("The environment variable %s is empty", r.Env),
			"it is set, but contains nothing except whitespace",
			fmt.Sprintf("Set %s to the secret's value and retry.", r.Env))
	}
	return New(s), nil
}

func (r Ref) resolveFile(baseDir string) (Value, []Warning, error) {
	path := r.File
	if !filepath.IsAbs(path) && baseDir != "" {
		path = filepath.Join(baseDir, path)
	}

	f, err := os.Open(path)
	if err != nil {
		return Value{}, nil, fileError(path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Value{}, nil, fileError(path, err)
	}
	if info.IsDir() {
		return Value{}, nil, fail.New("secret.file_unreadable",
			fmt.Sprintf("The secret file %s is a directory", path),
			"a secret reference must name a regular file",
			"Point `file` at the file that contains the secret.")
	}

	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return Value{}, nil, fileError(path, err)
	}
	if len(data) > maxFileSize {
		return Value{}, nil, fail.New("secret.file_unreadable",
			fmt.Sprintf("The secret file %s is too large", path),
			fmt.Sprintf("it exceeds %d bytes, which no token or key should", maxFileSize),
			"Check that `file` points at the secret itself and not at some other file.")
	}
	s := trimSecret(string(data))
	if s == "" {
		return Value{}, nil, fail.New("secret.empty",
			fmt.Sprintf("The secret file %s is empty", path),
			"it exists, but contains nothing except whitespace",
			"Write the secret into the file and retry.")
	}

	var warnings []Warning
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			warnings = append(warnings, Warning{
				Code:    WarnFilePermissions,
				Message: fmt.Sprintf("The secret file %s has mode %04o, so other users on this machine can read it.", path, perm),
				Next:    fmt.Sprintf("Run `chmod 600 %s`.", path),
			})
		}
	}
	return New(s), warnings, nil
}

func fileError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail.Wrap(err, "secret.file_not_found",
			fmt.Sprintf("The secret file %s does not exist", path),
			"the config file references it as the source of a secret",
			"Create the file, or correct the `file` path in the config file (relative paths are resolved against the config file's directory).")
	case errors.Is(err, fs.ErrPermission):
		return fail.Wrap(err, "secret.file_unreadable",
			fmt.Sprintf("The secret file %s cannot be read", path),
			"the current user does not have permission to read it",
			fmt.Sprintf("Fix its ownership or mode (for example `chmod 600 %s`) and retry.", path)).
			WithExit(fail.ExitPermission)
	default:
		return fail.Wrap(err, "secret.file_unreadable",
			fmt.Sprintf("The secret file %s cannot be read", path),
			Redact(err.Error()),
			"Check that the path names a readable regular file and retry.")
	}
}

// trimSecret removes the trailing newline and whitespace that editors and
// `echo` add. Leading whitespace is also dropped; no supported secret format
// begins with it.
func trimSecret(s string) string { return strings.TrimSpace(s) }
