package checklist_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/checklist"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

func checks(ran *[]string) []checklist.Check {
	mk := func(group, label string, r checklist.Result) checklist.Check {
		return checklist.Check{Group: group, Label: label, Run: func(context.Context) checklist.Result {
			*ran = append(*ran, label)
			return r
		}}
	}
	perms := mk("Microsoft Entra", "Application permissions", checklist.Fail("graph.permission_missing", "1 of 6 missing", "Grant admin consent, then run `taut auth` again."))
	perms.Expand = func() []checklist.Outcome {
		return []checklist.Outcome{
			{Label: "Teamwork.Migrate.All", Result: checklist.OK("import messages")},
			{Label: "Files.ReadWrite.All", Result: checklist.Fail("graph.permission_missing", "upload files", "")},
		}
	}
	return []checklist.Check{
		mk("Slack", "Token accepted", checklist.OK("acme (bot)")),
		mk("Slack", "Scopes", checklist.Warn("groups:read not granted", "Run `taut auth --slack-manifest` and update the app.")),
		mk("Microsoft Entra", "Signed in with certificate", checklist.OK("tenant 1f2e…")),
		perms,
		mk("Microsoft Entra", "Directory readable", checklist.Skip("needs User.Read.All")),
	}
}

func TestPlainOutput(t *testing.T) {
	var out, errb bytes.Buffer
	var ran []string
	got := checklist.Run(context.Background(), ui.Test(&out, &errb, false), "auth", "acme-migration", checks(&ran))

	if len(ran) != 5 {
		t.Fatalf("ran %d checks, want 5", len(ran))
	}
	if len(got) != 7 {
		t.Fatalf("got %d outcomes, want 7 (5 checks + 2 expanded)", len(got))
	}
	testutil.Golden(t, "plain", testutil.Stable(testutil.Plain(out.String())))
	if errb.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", errb.String())
	}
}

func TestJSONModeIsSilentAndOutcomesSerialize(t *testing.T) {
	var out, errb bytes.Buffer
	var ran []string
	got := checklist.Run(context.Background(), ui.Test(&out, &errb, true), "auth", "", checks(&ran))
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("JSON mode must print nothing itself; got %q / %q", out.String(), errb.String())
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status":"ok"`, `"status":"warning"`, `"status":"failed"`, `"status":"skipped"`, `"code":"graph.permission_missing"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON is missing %s: %s", want, b)
		}
	}
	s := checklist.Summarize(got)
	if s.OK != 2 || s.Warnings != 1 || s.Failed != 1 || s.Skipped != 1 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestCancelledContextSkipsRemainingChecks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	var ran []string
	got := checklist.Run(ctx, ui.Test(&out, &out, true), "auth", "", checks(&ran))
	if len(ran) != 0 {
		t.Fatalf("checks ran after cancellation: %v", ran)
	}
	for _, o := range got[:1] {
		if o.Status != theme.StatusSkip {
			t.Fatalf("status = %v, want skipped", o.Status)
		}
	}
}
