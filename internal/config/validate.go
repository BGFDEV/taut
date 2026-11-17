package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

var (
	reDNSLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reGUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reUPN      = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)
	reBucket   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

const zeroGUID = "00000000-0000-0000-0000-000000000000"

// Validate checks the whole config file and returns every finding; it never
// stops at the first problem. Errors must be fixed before the config file can be
// used; warnings are advisory. Validate is pure: it reads neither the
// environment nor the filesystem, so referenced secrets and files are checked
// for shape only.
//
// Use Issues.Err to turn the result into a single user-facing error.
func (m *Config) Validate() Issues {
	v := &validator{m: m}

	v.header()
	v.sourceSlack()
	v.entra()
	v.teams()
	v.identity()
	v.content()
	v.load()
	v.directMessages()
	if v.m.Local.Enabled {
		// Local mode has no bucket and no controller to reach; what the
		// file says about them is not used and not judged.
		v.local()
	} else {
		v.staging()
		v.controller()
		if v.m.Local != (Local{}) {
			v.warnf("local", "is set but local.enabled is false, so it has no effect")
		}
	}
	v.secrets()

	return v.issues
}

type validator struct {
	m      *Config
	issues Issues
}

// line returns the line of path, or of its nearest recorded ancestor.
func (v *validator) line(path string) int {
	if v.m.src == nil || v.m.src.lines == nil {
		return 0
	}
	for p := path; p != ""; {
		if l, ok := v.m.src.lines[p]; ok {
			return l
		}
		cut := strings.LastIndexAny(p, ".[")
		if cut < 0 {
			break
		}
		p = p[:cut]
	}
	return 0
}

func (v *validator) add(sev Severity, path, format string, args ...any) {
	v.issues = append(v.issues, Issue{
		Path: path, Severity: sev, Line: v.line(path),
		Message: fmt.Sprintf(format, args...),
	})
}

func (v *validator) errorf(path, format string, args ...any) {
	v.add(SeverityError, path, format, args...)
}

func (v *validator) warnf(path, format string, args ...any) {
	v.add(SeverityWarning, path, format, args...)
}

// enum checks that value is one of allowed.
func (v *validator) enum(path, value string, allowed ...string) bool {
	if value == "" {
		v.errorf(path, "is required; use one of: %s", strings.Join(allowed, ", "))
		return false
	}
	if contains(allowed, value) {
		return true
	}
	hint := ""
	if best := closest(value, allowed); best != "" {
		hint = fmt.Sprintf("; did you mean %q?", best)
	}
	v.errorf(path, "%s is not valid; use one of: %s%s", quote(value), strings.Join(allowed, ", "), hint)
	return false
}

// httpURL checks that value is an absolute http(s) URL without embedded
// credentials.
func (v *validator) httpURL(path, value string, required bool) {
	if value == "" {
		if required {
			v.errorf(path, "is required; use a URL such as http://localhost:9000")
		}
		return
	}
	u, err := url.Parse(value)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		v.errorf(path, "%s is not a valid URL; use the form http(s)://host[:port]", quote(value))
	case u.User != nil:
		v.errorf(path, "must not embed credentials in the URL; remove the user:password@ part and supply credentials through a secret reference")
	case u.RawQuery != "" || u.Fragment != "":
		v.errorf(path, "must not carry a query string or fragment; use the form http(s)://host[:port][/path]")
	}
}

// ref checks a secret reference.
func (v *validator) ref(path string, r secret.Ref, required bool) {
	if r.IsZero() {
		if required {
			v.errorf(path, "is required; reference the secret with `{ env: NAME }` or `{ file: path }`")
		}
		return
	}
	if _, literal := secret.LooksLikeSecret(r.Env + "\n" + r.File); literal {
		return // reported by the secret scan, with the remedy
	}
	if err := r.Validate(); err != nil {
		var fe *fail.Error
		if errors.As(err, &fe) {
			v.errorf(path, "%s", fe.Why)
			return
		}
		v.errorf(path, "%s", secret.Redact(err.Error()))
	}
}

func (v *validator) upn(path, value, what string) {
	if !reUPN.MatchString(value) {
		v.errorf(path, "%s is not a valid user principal name; use the form user@domain.com for %s", quote(value), what)
	}
}

func (v *validator) guid(path, value, what string) {
	switch {
	case value == "":
		v.errorf(path, "is required; set it to %s (a GUID such as 3f2504e0-4f89-11d3-9a0c-0305e82c3301)", what)
	case !reGUID.MatchString(value):
		v.errorf(path, "%s is not a GUID; set it to %s, in the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", quote(value), what)
	case value == zeroGUID:
		v.warnf(path, "is still the all-zero placeholder; set it to %s", what)
	}
}

func (v *validator) header() {
	m := v.m
	switch {
	case m.APIVersion == "":
		v.errorf("apiVersion", "is required; set it to %s", APIVersion)
	case m.APIVersion != APIVersion:
		v.errorf("apiVersion", "%s is not supported by this version of taut; use %s", quote(m.APIVersion), APIVersion)
	}
	switch {
	case m.Kind == "":
		v.errorf("kind", "is required; set it to %s", Kind)
	case m.Kind != Kind:
		v.errorf("kind", "%s is not valid; use %s", quote(m.Kind), Kind)
	}
	switch name := m.Metadata.Name; {
	case name == "":
		v.errorf("metadata.name", "is required; choose a short name such as acme-migration")
	case !reDNSLabel.MatchString(name):
		v.errorf("metadata.name", "%s is not a valid name; use 1–63 lowercase letters, digits and hyphens, starting and ending with a letter or digit", quote(name))
	}
}

func (v *validator) sourceSlack() {
	s := v.m.Source.Slack
	if strings.TrimSpace(s.Export) == "" {
		v.errorf("source.slack.export", "is required; set it to the path of the Slack export ZIP or its extracted directory")
	}
	if s.API.Enabled && s.API.Token.IsZero() {
		v.errorf("source.slack.api.token", "is required when api.enabled is true; reference the Slack token with `{ env: TAUT_SLACK_TOKEN }`, or set enabled to false")
	} else {
		v.ref("source.slack.api.token", s.API.Token, false)
	}
	v.httpURL("source.slack.api.baseURL", s.API.BaseURL, false)
}

func (v *validator) entra() {
	e := v.m.Target.Entra
	v.guid("target.entra.tenantId", e.TenantID, "the Entra tenant (directory) ID")
	v.guid("target.entra.clientId", e.ClientID, "the app registration's application (client) ID")
	v.enum("target.entra.cloud", e.Cloud, CloudGlobal, CloudUSGov, CloudUSGovHigh, CloudDoD, CloudChina)

	cert, sec := e.Credential.Certificate, e.Credential.ClientSecret
	switch {
	case cert.IsZero() && sec.IsZero():
		v.errorf("target.entra.credential", "is required; set exactly one of certificate (preferred, e.g. `certificate: { file: ./taut-app.pem }`) or clientSecret")
	case !cert.IsZero() && !sec.IsZero():
		v.errorf("target.entra.credential", "sets both certificate and clientSecret; keep exactly one (the certificate is preferred)")
	case !sec.IsZero():
		v.warnf("target.entra.credential.clientSecret", "a client secret works, but a certificate is preferred: it cannot be replayed if intercepted. Switch to `certificate: { file: … }` when you can")
	}
	v.ref("target.entra.credential.certificate", cert, false)
	v.ref("target.entra.credential.clientSecret", sec, false)

	v.httpURL("target.entra.authority", e.Authority, false)
	v.httpURL("target.entra.graphBaseURL", e.GraphBaseURL, false)
}

func (v *validator) teams() {
	teams := v.m.Target.Teams
	if len(teams) == 0 {
		v.errorf("target.teams", "needs at least one team; add an entry with name, owner and channels")
		return
	}

	firstByName := map[string]int{}
	for i, t := range teams {
		base := fmt.Sprintf("target.teams[%d]", i)

		name := strings.TrimSpace(t.Name)
		switch {
		case name == "":
			v.errorf(base+".name", "is required; set the Teams display name")
		case len([]rune(name)) > 256:
			v.errorf(base+".name", "is longer than the 256 characters Teams allows")
		default:
			key := strings.ToLower(name)
			if first, dup := firstByName[key]; dup {
				v.errorf(base+".name", "%s is also the name of target.teams[%d]; team names must be unique", quote(name), first)
			} else {
				firstByName[key] = i
			}
		}

		if t.Owner == "" {
			v.errorf(base+".owner", "is required; set it to the UPN of the team's initial owner, such as admin@acme.com")
		} else {
			v.upn(base+".owner", t.Owner, "the team's initial owner")
		}
		v.enum(base+".visibility", t.Visibility, VisibilityPrivate, VisibilityPublic)
		v.channels(base+".channels", t.Channels)
	}
	v.overlaps()
}

func (v *validator) channels(base string, c ChannelSelection) {
	globs := func(field string, patterns []string) {
		seen := map[string]bool{}
		for i, p := range patterns {
			at := fmt.Sprintf("%s.%s[%d]", base, field, i)
			switch {
			case p == "":
				v.errorf(at, "is empty; use a channel name or a glob such as proj-*")
			case !validGlob(p):
				v.errorf(at, "%s is not a valid glob pattern; check for an unclosed [ or a trailing \\", quote(p))
			case seen[p]:
				v.warnf(at, "%s is listed more than once", quote(p))
			}
			seen[p] = true
		}
	}
	if len(c.Include) == 0 {
		v.errorf(base+".include", `is empty, so the team would receive no channels; use ["*"] to include every channel`)
	}
	globs("include", c.Include)
	globs("exclude", c.Exclude)

	if len(c.Types) == 0 {
		v.errorf(base+".types", "is empty; list public, private or both")
	}
	seen := map[string]bool{}
	for i, t := range c.Types {
		at := fmt.Sprintf("%s.types[%d]", base, i)
		if v.enum(at, t, ChannelPublic, ChannelPrivate) && seen[t] {
			v.warnf(at, "%s is listed more than once", quote(t))
		}
		seen[t] = true
	}
	if v.m.Load.Strategy == StrategyLegacy && contains(c.Types, ChannelPrivate) {
		v.errorf(base+".types", "includes private, but the legacy load strategy supports standard (public) channels only; remove private here or set load.strategy to modern")
	}

	froms := make([]string, 0, len(c.Rename))
	for from := range c.Rename {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	targets := map[string]string{}
	for _, from := range froms {
		to := c.Rename[from]
		at := joinPath(base+".rename", safeSegment(from))
		switch {
		case strings.TrimSpace(from) == "":
			v.errorf(base+".rename", "has an empty Slack channel name as a key")
		case strings.TrimSpace(to) == "":
			v.errorf(at, "has no new name; set the Teams channel name or remove the entry")
		default:
			key := strings.ToLower(strings.TrimSpace(to))
			if other, dup := targets[key]; dup {
				v.errorf(at, "renames to %s, which %s is already renamed to; channel names within a team must be unique", quote(to), quote(other))
			} else {
				targets[key] = from
			}
		}
	}
}

// overlaps warns when two teams list the same include pattern for the same
// channel kind. Which channels exist is unknown here, so this is a static
// hint; the preflight reports the channels that are actually claimed twice.
func (v *validator) overlaps() {
	teams := v.m.Target.Teams
	for j := 1; j < len(teams); j++ {
		for i := 0; i < j; i++ {
			if !shareAny(teams[i].Channels.Types, teams[j].Channels.Types) {
				continue
			}
			for k, p := range teams[j].Channels.Include {
				if p == "" || !contains(teams[i].Channels.Include, p) {
					continue
				}
				v.warnf(fmt.Sprintf("target.teams[%d].channels.include[%d]", j, k),
					"%s is also included by target.teams[%d]; a Slack channel can go to only one team, so make sure the exclude lists keep the two selections apart", quote(p), i)
			}
		}
	}
}

func (v *validator) identity() {
	u := v.m.Identity.Unmapped
	if !v.enum("identity.unmapped.policy", u.Policy, PolicyFormerMember, PolicyServiceAccount, PolicyBlock) {
		return
	}
	switch {
	case u.Policy == PolicyBlock && u.Account != "":
		v.warnf("identity.unmapped.account", "is ignored because the policy is block; remove it or choose former-member or service-account")
	case u.Policy != PolicyBlock && u.Account == "":
		v.errorf("identity.unmapped.account", "is required when the policy is %s; set it to the UPN of the account that stands in for unmapped users, or set the policy to block", u.Policy)
	case u.Policy != PolicyBlock:
		v.upn("identity.unmapped.account", u.Account, "the stand-in account")
	}
}

func (v *validator) content() {
	c := v.m.Content
	v.enum("content.reactions", c.Reactions, ReactionsFooter, ReactionsNative, ReactionsDrop)
	if c.CustomEmoji == "image" {
		// Accepted by earlier versions, which did nothing with it.
		v.errorf("content.customEmoji", "cannot be image: a Slack export holds no emoji images and Taut has no way to fetch them, so a custom emoji is always written as its :name:; use %s", EmojiText)
	} else {
		v.enum("content.customEmoji", c.CustomEmoji, EmojiText)
	}

	date := func(path, value string) (ok bool) {
		if value == "" {
			return false
		}
		if _, err := ParseDate(value); err != nil {
			v.errorf(path, "%s is not a valid date; use YYYY-MM-DD, such as 2020-01-31", quote(value))
			return false
		}
		return true
	}
	sinceOK := date("content.since", c.Since)
	untilOK := date("content.until", c.Until)
	if sinceOK && untilOK && c.Since > c.Until {
		v.errorf("content.until", "%s is before content.since (%s); the range is inclusive, so until must be the same day or later", c.Until, c.Since)
	}
}

func (v *validator) load() {
	if !v.enum("load.strategy", v.m.Load.Strategy, StrategyModern, StrategyLegacy) {
		return
	}
	cloud := v.m.Target.Entra.Cloud
	national := contains([]string{CloudUSGov, CloudUSGovHigh, CloudDoD, CloudChina}, cloud)
	if v.m.Load.Strategy == StrategyModern && national {
		v.errorf("load.strategy", "modern is not available in the %s cloud: it relies on Graph's startMigration API, which exists only in the global cloud. Set load.strategy to legacy", cloud)
	}
}

// directMessages checks that direct messages can be migrated at all with
// the rest of the config file. They go into Teams chats, which are put into
// migration mode with Graph's startMigration API for chats: there is no way
// to create a chat in migration mode, so the legacy strategy has no path
// for them, and that API exists only in the global cloud.
func (v *validator) directMessages() {
	if !v.m.Target.DirectMessages.Enabled {
		return
	}
	const path = "target.directMessages.enabled"
	cloud := v.m.Target.Entra.Cloud
	switch {
	case contains([]string{CloudUSGov, CloudUSGovHigh, CloudDoD, CloudChina}, cloud):
		v.errorf(path, "direct messages cannot be migrated in the %s cloud: they are imported into Teams chats through Graph's startMigration API for chats, which exists only in the global cloud. Set target.directMessages.enabled to false; channels are unaffected", cloud)
	case v.m.Load.Strategy == StrategyLegacy:
		v.errorf(path, "direct messages cannot be migrated with the legacy load strategy: a chat cannot be created in migration mode, only put into it with Graph's startMigration API, which the legacy strategy does not use. Set target.directMessages.enabled to false, or set load.strategy to modern (global cloud only)")
	}
}

func (v *validator) staging() {
	s := v.m.Staging
	v.httpURL("staging.endpoint", s.Endpoint, true)
	if s.Region == "" {
		v.errorf("staging.region", "is required; use the object store's region, such as us-east-1")
	}
	switch {
	case s.Bucket == "":
		v.errorf("staging.bucket", "is required; set the name of the staging bucket")
	case !reBucket.MatchString(s.Bucket) || strings.Contains(s.Bucket, ".."):
		v.errorf("staging.bucket", "%s is not a valid bucket name; use 3–63 lowercase letters, digits, hyphens and dots", quote(s.Bucket))
	}
	v.ref("staging.accessKey", s.AccessKey, true)
	v.ref("staging.secretKey", s.SecretKey, true)
	v.ref("staging.encryptionKey", s.EncryptionKey, true)
}

func (v *validator) local() {
	l := v.m.Local
	if strings.ContainsRune(l.StateDir, 0) || strings.TrimSpace(l.StateDir) != l.StateDir {
		v.errorf("local.stateDir", "%s is not a usable directory path", quote(l.StateDir))
	}
	if l.Postgres.Binaries != "" && l.Postgres.Download {
		v.warnf("local.postgres.download", "has no effect while local.postgres.binaries names an installation")
	}
}

func (v *validator) controller() {
	if v.m.Controller.URL == "" {
		v.errorf("controller.url", "is required; use the controller address, such as http://localhost:7070")
		return
	}
	v.httpURL("controller.url", v.m.Controller.URL, true)
}

// secrets rejects any value, anywhere in the config file, that looks like a
// literal credential.
func (v *validator) secrets() {
	var tree yaml.Node
	if err := tree.Encode(v.m); err != nil {
		v.errorf("", "could not be inspected for literal secrets: %s", secret.Redact(err.Error()))
		return
	}
	v.issues = append(v.issues, scanSecrets(&tree, v.line)...)
}

func validGlob(pattern string) bool {
	_, err := path.Match(pattern, "")
	return err == nil
}

func shareAny(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}
