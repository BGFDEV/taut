package transform

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/slack"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// mapResolver is a Resolver backed by maps.
type mapResolver struct {
	users    map[string]UserRef
	channels map[string]string
}

func (r mapResolver) User(id string) (UserRef, bool) { u, ok := r.users[id]; return u, ok }

func (r mapResolver) Channel(id string) (string, bool) { n, ok := r.channels[id]; return n, ok }

// goldenResolver is the directory every golden case sees: mapped users,
// an unmapped one, and names that try to inject markup.
var goldenResolver = mapResolver{
	users: map[string]UserRef{
		"U1": {Name: "Ada Abbott", EntraID: "entra-ada"},
		"U2": {Name: "Ben Banerjee", EntraID: "entra-ben"},
		"U3": {Name: "Chloe Castro"},
		"U4": {Name: `<b>Eve</b> "O'Hara" & co`, EntraID: "entra-eve"},
		"U5": {Name: "</at><script>alert(1)</script>"},
		"U6": {Name: `<attachment id="taut-file:F1"></attachment>`, EntraID: "entra-marker"},
	},
	channels: map[string]string{
		"C1": "general",
		"C2": `<i>x</i>&"y"`,
		"C3": `<img src="taut-file:F1" alt="x">`,
	},
}

type goldenCase struct {
	Message slack.Message `json:"message"`
	Options struct {
		Reactions    string `json:"reactions"`
		MaxBytes     int    `json:"max_bytes"`
		AuthorMapped *bool  `json:"author_mapped"` // default true
		AuthorName   string `json:"author_name"`
		AuthorKind   string `json:"author_kind"`
		FileMode     string `json:"file_mode"`
		InlineMax    int64  `json:"inline_image_max"`
	} `json:"options"`
}

func (g goldenCase) options() Options {
	o := Options{
		Reactions:    g.Options.Reactions,
		MaxBytes:     g.Options.MaxBytes,
		AuthorMapped: true,
		AuthorName:   g.Options.AuthorName,
		AuthorKind:   g.Options.AuthorKind,

		FileMode:       g.Options.FileMode,
		InlineImageMax: g.Options.InlineMax,
	}
	if g.Options.AuthorMapped != nil {
		o.AuthorMapped = *g.Options.AuthorMapped
	}
	return o
}

// goldenText lays a Result out for a golden file: each part's HTML under a
// separator line, with blocks on their own lines for readable diffs, then
// what else the Result carries.
func goldenText(res Result) string {
	var sb strings.Builder
	for i, p := range res.Parts {
		fmt.Fprintf(&sb, "==================== part %d of %d (%d bytes) ====================\n", i+1, len(res.Parts), len(p.HTML))
		sb.WriteString(p.HTML)
		sb.WriteByte('\n')
		for _, m := range p.Mentions {
			fmt.Fprintf(&sb, "-- mention %d: %q -> %s\n", m.ID, m.Text, m.EntraID)
		}
		for _, f := range p.Files {
			fmt.Fprintf(&sb, "-- part file: %s\n", f)
		}
	}
	for _, r := range res.Reactions {
		fmt.Fprintf(&sb, "-- reaction: %s x%d\n", r.Emoji, r.Count)
	}
	for _, f := range res.Files {
		fmt.Fprintf(&sb, "-- file: id=%q name=%q title=%q mimetype=%q size=%d external=%t url=%q unavailable=%t canvas=%t\n",
			f.ID, f.Name, f.Title, f.Mimetype, f.Size, f.External, f.ExternalURL, f.Unavailable, f.Canvas)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&sb, "-- warning: %s\n", w)
	}
	return sb.String()
}

func TestGolden(t *testing.T) {
	inputs, err := filepath.Glob("testdata/golden/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) < 60 {
		t.Fatalf("found %d golden cases, want at least 60", len(inputs))
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			var g goldenCase
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("%s: %v", in, err)
			}
			o := g.options()
			res, err := Convert(g.Message, goldenResolver, o)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			limit := o.MaxBytes
			if limit == 0 {
				limit = DefaultMaxBytes
			}
			if err := checkResult(res, max(limit, MinMaxBytes)); err != nil {
				t.Errorf("invalid result: %v", err)
			}
			got := goldenText(res)
			path := strings.TrimSuffix(in, ".json") + ".html"
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}
