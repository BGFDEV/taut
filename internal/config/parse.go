package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

// Load reads and parses the config file at path. Relative paths inside it
// resolve against the file's directory (see ResolvePath).
//
// Errors are *fail.Error values: "config.not_found", "config.unreadable",
// "config.syntax" (with the line), "config.empty" or "config.invalid"
// (unknown fields, wrongly typed values and literal secrets, each with its
// line). Load does not run Validate.
func Load(path string) (*Config, error) {
	m, _, err := LoadFile(path)
	return m, err
}

// LoadFile is Load, and also returns the bytes the config file was parsed
// from. A caller that will write the file back uses them to tell whether
// the file changed in the meantime: the config file it edits and the bytes it
// compares against then come from one read.
func LoadFile(path string) (*Config, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fail.Wrap(err, "config.not_found",
				fmt.Sprintf("No config file found at %s", path),
				"the file does not exist",
				"Run `taut init` to create one, or pass --config <path>.").WithExit(fail.ExitUsage)
		}
		why := "the file could not be read"
		if errors.Is(err, fs.ErrPermission) {
			why = "the current user does not have permission to read it"
		} else if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			why = "the path is a directory, not a file"
		}
		return nil, nil, fail.Wrap(err, "config.unreadable",
			fmt.Sprintf("The config file %s cannot be read", path), why,
			"Check the path and its permissions, or pass --config <path> to use another file.").WithExit(fail.ExitUsage)
	}

	m, err := parse(data, path)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Dir(path)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	m.src.dir = dir
	return m, data, nil
}

// Parse decodes a config file from YAML. Decoding is strict: unknown fields,
// repeated fields, wrongly typed values and anything that looks like a
// literal secret are reported together, with line numbers, as a
// "config.invalid" *fail.Error (the secret itself is never echoed);
// malformed YAML is a "config.syntax" *fail.Error and an empty document is
// "config.empty". Omitted optional fields receive the values from Default.
// Parse does not run Validate.
//
// Relative paths in a parsed config resolve against the working directory
// until SetBaseDir is called; Load does that for you.
func Parse(data []byte) (*Config, error) {
	return parse(data, "")
}

func parse(data []byte, file string) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, syntaxError(file, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || isNull(doc.Content[0]) {
		return nil, fail.New("config.empty", "The config file"+fileSuffix(file)+" is empty",
			"it contains no YAML document",
			"Run `taut init` to create one, or pass --config <path>.").WithExit(fail.ExitUsage)
	}
	root := resolveAlias(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, fail.Wrap(&SyntaxError{Line: root.Line, Message: "the top level must be a mapping"},
			"config.syntax", "The config file"+fileSuffix(file)+" is not a valid config file",
			fmt.Sprintf("line %d: the top level must be a mapping of settings such as `apiVersion:` and `source:`", root.Line),
			"Run `taut init` to generate a well-formed config file.").WithExit(fail.ExitUsage)
	}

	// Pass 1: reject literal secrets anywhere in the tree, known field or
	// not, before any value can reach an error message.
	issues := scanSecrets(root, func(string) int { return 0 })

	// Pass 2: check the document's shape against the schema, recording
	// where every field sits.
	w := &walker{lines: map[string]int{}, set: map[string]bool{}}
	w.walk(root, reflect.TypeOf(Config{}), "")
	issues = append(issues, w.issues...)

	// Pass 3: decode. Conversion failures yaml reports are folded into
	// issues unless pass 1 or 2 already explained that line.
	m := &Config{}
	if err := root.Decode(m); err != nil {
		issues = append(issues, w.decodeIssues(err, issues)...)
	}
	if issues.HasErrors() {
		sortIssues(issues)
		return nil, issues.err(file)
	}

	w.applyDefaults(m)
	m.src = &source{file: file, lines: w.lines}
	return m, nil
}

func fileSuffix(file string) string {
	if file == "" {
		return ""
	}
	return " " + file
}

var reYAMLLine = regexp.MustCompile(`line (\d+)`)

func syntaxError(file string, err error) error {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	se := &SyntaxError{Message: secret.Redact(msg)}
	if m := reYAMLLine.FindStringSubmatch(msg); m != nil {
		se.Line, _ = strconv.Atoi(m[1])
		se.Message = secret.Redact(strings.TrimPrefix(strings.TrimPrefix(msg, m[0]), ": "))
	}
	return fail.Wrap(se, "config.syntax",
		"The config file"+fileSuffix(file)+" is not valid YAML",
		se.Error(),
		"Fix the YAML syntax at that line (check indentation, colons and quotes), or run `taut init` to generate a fresh config file.").
		WithExit(fail.ExitUsage)
}

// walker checks a YAML tree against the Go schema by reflection.
type walker struct {
	lines  map[string]int  // field path → line
	set    map[string]bool // field paths given a non-null value
	issues Issues
}

var refType = reflect.TypeOf(secret.Ref{})

func (w *walker) errorf(path string, line int, format string, args ...any) {
	w.issues = append(w.issues, Issue{
		Path: path, Line: line, Severity: SeverityError,
		Message: fmt.Sprintf(format, args...),
	})
}

func (w *walker) record(path string, line int, value *yaml.Node) {
	w.lines[path] = line
	if !isNull(value) {
		w.set[path] = true
	}
}

func (w *walker) walk(n *yaml.Node, t reflect.Type, path string) {
	n = resolveAlias(n)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if isNull(n) {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			w.kindMismatch(path, n, t)
			return
		}
		fields := yamlFields(t)
		seen := map[string]int{}
		kept := n.Content[:0:0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" {
				kept = append(kept, k, v)
				continue
			}
			p := joinPath(path, safeSegment(k.Value))
			if first, dup := seen[k.Value]; dup {
				w.errorf(p, k.Line, "is set more than once (first on line %d); keep one", first)
				continue
			}
			seen[k.Value] = k.Line
			kept = append(kept, k, v)
			ft, ok := fields[k.Value]
			if !ok {
				w.errorf(p, k.Line, "unknown field%s", didYouMean(k.Value, fieldNames(fields)))
				continue
			}
			w.record(p, k.Line, v)
			w.walk(v, ft, p)
		}
		// Drop repeated keys (already reported) so that the decoder, which
		// aborts on the first one, still reports everything else.
		n.Content = kept
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			w.kindMismatch(path, n, t)
			return
		}
		for i, item := range n.Content {
			p := fmt.Sprintf("%s[%d]", path, i)
			w.record(p, item.Line, item)
			w.walk(item, t.Elem(), p)
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			w.kindMismatch(path, n, t)
			return
		}
		seen := map[string]int{}
		kept := n.Content[:0:0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := joinPath(path, safeSegment(k.Value))
			if first, dup := seen[k.Value]; dup {
				w.errorf(p, k.Line, "is set more than once (first on line %d); keep one", first)
				continue
			}
			seen[k.Value] = k.Line
			kept = append(kept, k, v)
			w.record(p, k.Line, v)
			w.walk(v, t.Elem(), p)
		}
		n.Content = kept
	default:
		if n.Kind != yaml.ScalarNode {
			w.kindMismatch(path, n, t)
		}
	}
}

// kindMismatch reports a node of the wrong shape (say, a string where a
// mapping belongs). The offending value is never echoed.
func (w *walker) kindMismatch(path string, n *yaml.Node, t reflect.Type) {
	if n.Kind == yaml.ScalarNode {
		if _, ok := secret.LooksLikeSecret(n.Value); ok {
			return // the secret scan reports this node, more helpfully
		}
	}
	if t == refType {
		w.errorf(path, n.Line, "must be a secret reference such as `{ env: NAME }` or `{ file: path }`, not %s", describeNode(n))
		return
	}
	w.errorf(path, n.Line, "must be %s, not %s", describeType(t), describeNode(n))
}

func describeType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return "a mapping"
	case reflect.Slice:
		return "a list"
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "a single value"
	default:
		return "a " + t.Kind().String()
	}
}

func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	default:
		return describeTag(n.ShortTag())
	}
}

func describeTag(tag string) string {
	switch tag {
	case "!!str":
		return "a string"
	case "!!int", "!!float":
		return "a number"
	case "!!bool":
		return "a boolean"
	case "!!seq":
		return "a list"
	case "!!map":
		return "a mapping"
	case "!!timestamp":
		return "a date"
	case "!!null":
		return "nothing"
	default:
		return "a value of another type"
	}
}

var (
	reCannotUnmarshal = regexp.MustCompile("(?s)^line (\\d+): cannot unmarshal (!!\\w+)(?: `.*`)? into (.+)$")
	reAlreadySet      = regexp.MustCompile(`^line (\d+): (?:field (\S+) already set|mapping key .* already defined)`)
	reLinePrefix      = regexp.MustCompile(`^line (\d+): `)
)

// decodeIssues converts a yaml decode error into issues, skipping lines that
// already have one. Values quoted by yaml are dropped from the messages.
func (w *walker) decodeIssues(err error, existing Issues) Issues {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return Issues{{Severity: SeverityError, Message: secret.Redact(err.Error())}}
	}
	explained := map[int]bool{}
	for _, i := range existing {
		explained[i.Line] = true
	}
	byLine := w.pathsByLine()

	var out Issues
	for _, msg := range te.Errors {
		issue := Issue{Severity: SeverityError}
		switch {
		case reCannotUnmarshal.MatchString(msg):
			m := reCannotUnmarshal.FindStringSubmatch(msg)
			issue.Line, _ = strconv.Atoi(m[1])
			issue.Message = fmt.Sprintf("must be %s, not %s", describeGoType(m[3]), describeTag(m[2]))
		case reAlreadySet.MatchString(msg):
			m := reAlreadySet.FindStringSubmatch(msg)
			issue.Line, _ = strconv.Atoi(m[1])
			issue.Message = "is set more than once; keep one"
		default:
			if m := reLinePrefix.FindStringSubmatch(msg); m != nil {
				issue.Line, _ = strconv.Atoi(m[1])
				msg = strings.TrimPrefix(msg, m[0])
			}
			issue.Message = secret.Redact(msg)
		}
		if issue.Line > 0 && explained[issue.Line] {
			continue
		}
		issue.Path = byLine[issue.Line]
		out = append(out, issue)
	}
	return out
}

func describeGoType(t string) string {
	switch {
	case t == "bool":
		return "true or false"
	case t == "string":
		return "a string"
	case strings.HasPrefix(t, "[]"):
		return "a list"
	case strings.HasPrefix(t, "map["):
		return "a mapping"
	case strings.HasPrefix(t, "int"), strings.HasPrefix(t, "uint"), strings.HasPrefix(t, "float"):
		return "a number"
	default:
		return "a mapping"
	}
}

// pathsByLine inverts lines, preferring the deepest path on each line.
func (w *walker) pathsByLine() map[int]string {
	out := map[int]string{}
	for p, line := range w.lines {
		if cur, ok := out[line]; !ok || len(p) > len(cur) || (len(p) == len(cur) && p < cur) {
			out[line] = p
		}
	}
	return out
}

// applyDefaults fills every field the document left out (or set to null)
// from Default, then applies the per-team defaults.
func (w *walker) applyDefaults(m *Config) {
	w.merge(reflect.ValueOf(m).Elem(), reflect.ValueOf(Default()).Elem(), "")

	def := DefaultTeam()
	for i := range m.Target.Teams {
		t := &m.Target.Teams[i]
		base := fmt.Sprintf("target.teams[%d]", i)
		if !w.set[base+".visibility"] {
			t.Visibility = def.Visibility
		}
		if !w.set[base+".channels.include"] {
			t.Channels.Include = append([]string(nil), def.Channels.Include...)
		}
		if !w.set[base+".channels.types"] {
			t.Channels.Types = append([]string(nil), def.Channels.Types...)
		}
		// Normalize empties so a config file survives Marshal → Parse unchanged.
		if len(t.Channels.Exclude) == 0 {
			t.Channels.Exclude = nil
		}
		if len(t.Channels.Rename) == 0 {
			t.Channels.Rename = nil
		}
	}
}

func (w *walker) merge(dst, def reflect.Value, path string) {
	t := dst.Type()
	for i := 0; i < t.NumField(); i++ {
		name, ok := yamlName(t.Field(i))
		if !ok {
			continue
		}
		p := joinPath(path, name)
		f := dst.Field(i)
		if f.Kind() == reflect.Struct && f.Type() != refType {
			w.merge(f, def.Field(i), p)
			continue
		}
		if !w.set[p] {
			f.Set(def.Field(i))
		}
	}
}

// scanSecrets walks every scalar (keys included) under n and returns an error
// issue for each one that looks like a literal credential. The issue names
// the path and line, never the value. lineOf supplies the line when the node
// itself has none (a tree encoded from a struct).
func scanSecrets(n *yaml.Node, lineOf func(path string) int) Issues {
	var out Issues
	report := func(path string, node *yaml.Node, kind string) {
		line := node.Line
		if line == 0 {
			line = lineOf(path)
		}
		out = append(out, Issue{
			Path: path, Line: line, Severity: SeverityError,
			Message: fmt.Sprintf("looks like a literal %s. The config file must not contain secrets: "+
				"move it to an environment variable or a file and reference that instead, "+
				"for example `{ env: NAME }` or `{ file: path }`", kind),
		})
	}
	var visit func(n *yaml.Node, path string, depth int)
	visit = func(n *yaml.Node, path string, depth int) {
		if n == nil || depth > 64 {
			return
		}
		n = resolveAlias(n)
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				visit(c, path, depth+1)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				p := joinPath(path, safeSegment(k.Value))
				if kind, ok := secret.LooksLikeSecret(k.Value); ok {
					report(p, k, kind)
				}
				visit(v, p, depth+1)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				visit(c, fmt.Sprintf("%s[%d]", path, i), depth+1)
			}
		case yaml.ScalarNode:
			if kind, ok := secret.LooksLikeSecret(n.Value); ok {
				report(path, n, kind)
			}
		}
	}
	visit(n, "", 0)
	return out
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && n.Alias != nil && i < 16; i++ {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

// safeSegment makes a mapping key safe to show in a path.
func safeSegment(key string) string {
	if _, ok := secret.LooksLikeSecret(key); ok {
		return secret.Marker
	}
	return secret.Redact(key)
}

// quote renders a user-supplied value for a message: redacted, truncated and
// quoted.
func quote(v string) string {
	v = secret.Redact(v)
	if r := []rune(v); len(r) > 60 {
		v = string(r[:57]) + "…"
	}
	return strconv.Quote(v)
}

func yamlName(f reflect.StructField) (string, bool) {
	if !f.IsExported() {
		return "", false
	}
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "-" {
		return "", false
	}
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name, true
}

func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if name, ok := yamlName(t.Field(i)); ok {
			out[name] = t.Field(i).Type
		}
	}
	return out
}

func fieldNames(fields map[string]reflect.Type) []string {
	out := make([]string, 0, len(fields))
	for name := range fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// didYouMean returns `; did you mean "x"?` when one candidate is close to
// name, and otherwise lists what is allowed.
func didYouMean(name string, candidates []string) string {
	if best := closest(name, candidates); best != "" {
		return fmt.Sprintf("; did you mean %q?", best)
	}
	if len(candidates) == 0 {
		return ""
	}
	return "; expected one of: " + strings.Join(candidates, ", ")
}

// closest returns the candidate nearest to name, or "" if none is near.
func closest(name string, candidates []string) string {
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
	}
	n := norm(name)
	if n == "" {
		return ""
	}
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		d := levenshtein(n, norm(c))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	limit := len(n) / 3
	if limit < 1 {
		limit = 1
	}
	if limit > 3 {
		limit = 3
	}
	if bestDist > limit {
		return ""
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func sortIssues(is Issues) {
	sort.SliceStable(is, func(a, b int) bool {
		la, lb := is[a].Line, is[b].Line
		if la == 0 || lb == 0 {
			return la != 0 && lb == 0 // unknown lines last
		}
		return la < lb
	})
}
