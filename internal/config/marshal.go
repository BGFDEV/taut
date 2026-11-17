package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bgfdev/taut/internal/fail"
)

const headerComment = `Taut config file: the plan for migrating a Slack workspace to Microsoft Teams.

This file contains no secrets and is safe to commit. Credentials are named by
reference, as { env: NAME } or { file: path }, and read only when needed.
Relative paths are resolved against this file's directory.

Generate or regenerate it with ` + "`taut init`" + `; check it with ` + "`taut preflight`" + `.`

// sectionComments are the comments Marshal puts above top-level sections.
var sectionComments = map[string]string{
	"source":     "Where the data comes from: a Slack export, optionally enriched via the Slack API.",
	"target":     "Where it goes: the Entra tenant, and the Teams to create with their channels.",
	"identity":   "How Slack users map to Entra users, and what happens to those with no match.",
	"content":    "How messages are converted, and the optional date range to migrate.",
	"load":       "How messages are imported: modern (global cloud) or legacy (national clouds; standard channels only).",
	"staging":    "Object store for staged data. Everything is encrypted client-side before upload.",
	"controller": "The controller that runs the migration.",
	"local":      "Local mode: this one taut process runs the whole migration, with its state in a directory beside this file.",
}

// Marshal renders the config file as stable, commented YAML with a two-space
// indent. Test-override fields and other empty optional fields are omitted.
// The output parses back, with Parse, to an equal config file.
func (m *Config) Marshal() ([]byte, error) {
	var root yaml.Node
	if err := root.Encode(m); err != nil {
		return nil, fail.Wrap(err, "config.encode", "The config file could not be encoded as YAML", "",
			"This is a bug in taut; please report it with the output of `taut doctor`.")
	}
	decorate(&root, "")
	if m.Local.Enabled {
		// A local-mode file says nothing about a bucket or a controller:
		// there is neither. Parse fills the defaults back in, unused.
		kept := root.Content[:0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if k := root.Content[i].Value; k != "staging" && k != "controller" {
				kept = append(kept, root.Content[i], root.Content[i+1])
			}
		}
		root.Content = kept
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if c, ok := sectionComments[root.Content[i].Value]; ok {
			root.Content[i].HeadComment = c
		}
	}

	var buf bytes.Buffer
	for _, line := range strings.Split(headerComment, "\n") {
		buf.WriteString(strings.TrimRight("# "+line, " "))
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')

	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fail.Wrap(err, "config.encode", "The config file could not be encoded as YAML", "",
			"This is a bug in taut; please report it with the output of `taut doctor`.")
	}
	if err := enc.Close(); err != nil {
		return nil, fail.Wrap(err, "config.encode", "The config file could not be encoded as YAML", "",
			"This is a bug in taut; please report it with the output of `taut doctor`.")
	}

	// Separate the top-level sections with a blank line.
	prevBlank := true
	for _, line := range strings.SplitAfter(body.String(), "\n") {
		if strings.HasPrefix(line, "# ") && !prevBlank {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
		prevBlank = strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#")
	}
	return buf.Bytes(), nil
}

// decorate adjusts node styles for readability: secret references and short
// string lists are written inline, and dates are written unquoted.
func decorate(n *yaml.Node, key string) {
	switch n.Kind {
	case yaml.MappingNode:
		if isRefNode(n) {
			n.Style = yaml.FlowStyle
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			decorate(n.Content[i+1], n.Content[i].Value)
		}
	case yaml.SequenceNode:
		scalars := len(n.Content) > 0
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				scalars = false
			}
			decorate(c, "")
		}
		if scalars && len(n.Content) <= 8 {
			n.Style = yaml.FlowStyle
		}
	case yaml.ScalarNode:
		if key == "since" || key == "until" {
			if _, err := ParseDate(n.Value); err == nil {
				n.Tag, n.Style = "!!timestamp", 0
			}
		}
	}
}

// isRefNode reports whether n is an encoded secret.Ref: a non-empty mapping
// with no keys other than env and file.
func isRefNode(n *yaml.Node) bool {
	if len(n.Content) == 0 || len(n.Content) > 4 {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if v.Kind != yaml.ScalarNode || (k.Value != "env" && k.Value != "file") {
			return false
		}
	}
	return true
}

// Write saves the config file to path with mode 0600. The write is atomic: the
// content goes to a temporary file in the same directory, which is then
// moved into place. An existing file is replaced only when overwrite is
// true; otherwise Write returns a "config.exists" *fail.Error and leaves
// the file untouched.
func (m *Config) Write(path string, overwrite bool) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}

	exists := func() error {
		return fail.New("config.exists",
			fmt.Sprintf("A config file already exists at %s", path),
			"taut does not replace an existing config file unless told to",
			"Pass --force to overwrite it, or choose another location with --config <path>.").
			WithExit(fail.ExitUsage)
	}
	writeFailed := func(err error) error {
		why := ""
		switch {
		case errors.Is(err, fs.ErrNotExist):
			why = "the directory does not exist"
		case errors.Is(err, fs.ErrPermission):
			why = "the current user does not have permission to write there"
		}
		return fail.Wrap(err, "config.write_failed",
			fmt.Sprintf("The config file could not be written to %s", path), why,
			"Check that the directory exists and is writable, or choose another location with --config <path>.")
	}

	if !overwrite {
		if _, err := os.Lstat(path); err == nil {
			return exists()
		}
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return writeFailed(err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close()
		return writeFailed(err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return writeFailed(err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return writeFailed(err)
	}
	if err := tmp.Close(); err != nil {
		return writeFailed(err)
	}

	if !overwrite {
		// A hard link fails if the destination appeared since the check
		// above, so a concurrent writer is never clobbered.
		switch err := os.Link(tmpName, path); {
		case err == nil:
			return nil // the deferred cleanup removes the temporary name
		case errors.Is(err, fs.ErrExist):
			return exists()
		}
		// Linking is unsupported here; fall back to rename.
		if _, err := os.Lstat(path); err == nil {
			return exists()
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return writeFailed(err)
	}
	committed = true
	return nil
}
