package egressproxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// Scopes a destination can be classified into, which decide both its rate
// budget and which credentials it may receive:
//
//   - ScopeGraph: Microsoft Graph. Budget "graph". Takes an Entra token, and
//     only when the host is the Graph origin of the request's own auth
//     descriptor.
//   - ScopeSharePoint: SharePoint upload sessions. Budget "sharepoint".
//     Pre-authorized URLs only (kind none).
//   - ScopeSlack: the Slack Web API. Budget "slack". Takes a Slack token on
//     paths under /api/.
//   - ScopeSlackFiles: Slack file downloads. Budget "slack". Takes a Slack
//     token on GET and HEAD.
//   - ScopeOther: anything else the operator added. Not rate limited, and
//     never given a credential (kind none only).
const (
	ScopeGraph      = budget.ScopeGraph
	ScopeSharePoint = budget.ScopeSharePoint
	ScopeSlack      = budget.ScopeSlack
	ScopeSlackFiles = "slackfiles"
	ScopeOther      = "other"
)

// budgetScope maps a destination scope to the budget it draws on.
func budgetScope(scope string) string {
	if scope == ScopeSlackFiles {
		return ScopeSlack
	}
	return scope
}

// DefaultAllowHosts returns the destinations every egress proxy accepts, as
// AllowHosts entries.
func DefaultAllowHosts() []string {
	return []string{
		"graph=graph.microsoft.com",
		"graph=graph.microsoft.us",
		"graph=dod-graph.microsoft.us",
		"graph=microsoftgraph.chinacloudapi.cn",
		"sharepoint=*.sharepoint.com",
		"sharepoint=*.sharepoint.us",
		"sharepoint=*.sharepoint.cn",
		"slack=slack.com",
		"slackfiles=files.slack.com",
		"slackfiles=*.slack-files.com",
	}
}

// hostRule is one parsed AllowHosts entry.
type hostRule struct {
	scheme string // "https" or "http"
	host   string // exact host, lower case; empty for a wildcard
	suffix string // ".sharepoint.com" for "*.sharepoint.com"
	port   string // empty means the scheme's default port only
	scope  string
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// parseRule parses "[scope=][scheme://]host[:port]", where host is a name,
// "*.suffix" or an IP literal.
func parseRule(entry string) (hostRule, error) {
	r := hostRule{scheme: "https", scope: ScopeOther}
	rest := strings.TrimSpace(entry)
	if scope, after, ok := strings.Cut(rest, "="); ok {
		switch scope {
		case ScopeGraph, ScopeSharePoint, ScopeSlack, ScopeSlackFiles, ScopeOther:
			r.scope = scope
		default:
			return r, fmt.Errorf("allow entry %q: unknown scope %q (use graph, sharepoint, slack, slackfiles or other)", entry, scope)
		}
		rest = after
	}
	if scheme, after, ok := strings.Cut(rest, "://"); ok {
		if scheme != "http" && scheme != "https" {
			return r, fmt.Errorf("allow entry %q: scheme must be http or https", entry)
		}
		r.scheme, rest = scheme, after
	}
	host := strings.ToLower(rest)
	if h, p, err := net.SplitHostPort(host); err == nil {
		if p == "" {
			return r, fmt.Errorf("allow entry %q: empty port", entry)
		}
		host, r.port = h, p
	} else {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	switch {
	case strings.HasPrefix(host, "*."):
		if !hostnameRE.MatchString(host[2:]) || !strings.Contains(host[2:], ".") {
			return r, fmt.Errorf("allow entry %q: a wildcard needs a domain of at least two labels after \"*.\"", entry)
		}
		r.suffix = host[1:]
	case hostnameRE.MatchString(host):
		r.host = host
	default:
		addr, err := netip.ParseAddr(host)
		if err != nil {
			return r, fmt.Errorf("allow entry %q: %q is not a host name, wildcard or IP address", entry, host)
		}
		r.host = addr.String()
	}
	return r, nil
}

// allowlist decides which upstream origins requests may be forwarded to.
type allowlist struct{ rules []hostRule }

func newAllowlist(extra []string) (*allowlist, error) {
	a := &allowlist{}
	for _, e := range append(DefaultAllowHosts(), extra...) {
		r, err := parseRule(e)
		if err != nil {
			return nil, err
		}
		a.rules = append(a.rules, r)
	}
	return a, nil
}

// parseUpstream parses the X-Taut-Upstream header: an origin and nothing
// more.
func parseUpstream(raw string) (*url.URL, error) {
	if len(raw) > 512 {
		return nil, errors.New("it is too long")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("it is not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("its scheme must be https")
	case u.Host == "" || u.Opaque != "":
		return nil, errors.New("it has no host")
	case u.User != nil:
		return nil, errors.New("it must not contain credentials")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, errors.New("it must be an origin (scheme://host[:port]) without path or query; the path travels in the request line")
	case u.Hostname() == "":
		return nil, errors.New("it has no host")
	}
	u.Path, u.RawPath = "", ""
	return u, nil
}

// check returns the scope of the destination, or why it is refused.
func (a *allowlist) check(u *url.URL) (scope string, err error) {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	isIP := false
	if addr, perr := netip.ParseAddr(host); perr == nil {
		isIP, host = true, addr.String()
	} else if !hostnameRE.MatchString(host) {
		return "", errors.New("the host is not a valid DNS name")
	}

	hostListed := false
	for _, r := range a.rules {
		var hostOK bool
		if r.suffix != "" {
			hostOK = !isIP && strings.HasSuffix(host, r.suffix) && len(host) > len(r.suffix)
		} else {
			hostOK = host == r.host
		}
		if !hostOK {
			continue
		}
		hostListed = true
		want := r.port
		if want == "" {
			want = defaultPort(r.scheme)
		}
		if r.scheme == u.Scheme && want == port {
			return r.scope, nil
		}
	}
	switch {
	case hostListed && u.Scheme != "https":
		return "", errors.New("only https is allowed for this host")
	case hostListed:
		return "", errors.New("the host is allowed, but not on this scheme and port")
	case isIP:
		return "", errors.New("IP addresses are not allowed as destinations")
	default:
		return "", errors.New("the host is not on the egress proxy's list of destinations")
	}
}

// originOf renders u's origin in a canonical form for comparison.
func originOf(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.String()
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port)
}

// checkPairing decides whether the credential a describes may be attached
// to this request. It runs before the token broker is asked for anything.
//
//   - An Entra token goes only to the Graph origin its own descriptor
//     resolves to: graphBase if set, else the cloud's Graph base, and only
//     if that host is classified graph. Never to SharePoint (upload sessions
//     are pre-authorized), another cloud's Graph, or an unscoped host.
//   - A Slack token goes to the Web API (paths under /api/) or, for GET and
//     HEAD, to a file host.
//   - Unscoped operator-added hosts get no credential at all.
func checkPairing(a wire.Auth, scope, method, urlPath string, upstream *url.URL) error {
	switch a.Kind {
	case wire.KindNone:
		return nil
	case wire.KindEntra:
		if scope != ScopeGraph {
			return errors.New("Entra tokens are sent only to Microsoft Graph; this host is not classified as Graph (SharePoint upload sessions use kind \"none\")")
		}
		base := strings.TrimSpace(a.GraphBase)
		if base == "" {
			cloud, err := graph.LookupCloud(a.Cloud)
			if err != nil {
				return errors.New("the auth descriptor names an unknown cloud")
			}
			base = cloud.GraphBase
		}
		want, err := parseUpstream(strings.TrimRight(base, "/"))
		if err != nil {
			return errors.New("the auth descriptor's graphBase is not an origin")
		}
		if originOf(want) != originOf(upstream) {
			return errors.New("an Entra token is sent only to the Graph origin of its own descriptor (" + want.Host + "), not to another host")
		}
		return nil
	case wire.KindSlack:
		switch scope {
		case ScopeSlackFiles:
			if method != http.MethodGet && method != http.MethodHead {
				return errors.New("a Slack token is sent to a file host only for GET and HEAD")
			}
			return nil
		case ScopeSlack:
			if !strings.HasPrefix(urlPath, "/api/") || strings.Contains(urlPath, "..") {
				return errors.New("a Slack token is sent to the Slack API only for paths under /api/")
			}
			return nil
		}
		return errors.New("Slack tokens are sent only to Slack API and file hosts")
	}
	return errors.New("unknown auth kind")
}

// chatKeyPrefix marks the key of a chat. A team ID is a GUID, so no
// channel key can begin with it.
const chatKeyPrefix = "chat:"

// isChatKey reports whether a key from channelKey names a chat.
func isChatKey(key string) bool { return strings.HasPrefix(key, chatKeyPrefix) }

// channelKey returns the conversation a request imports a message into:
// "{team}/{channel}" when it posts a message or a reply into a Teams
// channel, "chat:{chat}" when it posts a message into a chat, and ""
// otherwise. escapedPath is the request path as sent (still
// percent-encoded).
//
// A channel and a chat each have their own rate limit at Microsoft (one
// message a second by default), which is why each gets its own bucket.
//
// The key is what the limit hangs on, so a path Graph would serve the same
// way must give the same key: Graph's path segments are not case
// sensitive, and it ignores a trailing or doubled slash. Segments are
// therefore compared without regard to case, empty ones are dropped, and
// the key is lower case. Two spellings of one conversation share a bucket;
// at worst two conversations whose IDs differ only by case would too,
// which errs on the slow side.
func channelKey(method, scope, escapedPath string) string {
	if method != http.MethodPost || scope != ScopeGraph {
		return ""
	}
	var seg []string
	for _, s := range strings.Split(escapedPath, "/") {
		if s != "" {
			seg = append(seg, s)
		}
	}
	is := func(i int, word string) bool { return strings.EqualFold(seg[i], word) }
	if len(seg) < 4 || !(is(0, "v1.0") || is(0, "beta")) {
		return ""
	}
	id := func(i int) string {
		v, err := url.PathUnescape(seg[i])
		if err != nil {
			return ""
		}
		return strings.ToLower(v)
	}
	switch {
	// version, "chats", c, "messages"
	case len(seg) == 4 && is(1, "chats") && is(3, "messages"):
		if chat := id(2); chat != "" {
			return chatKeyPrefix + chat
		}
	// version, "teams", t, "channels", c, "messages" [, m, "replies"]
	case (len(seg) == 6 || len(seg) == 8) && is(1, "teams") && is(3, "channels") && is(5, "messages"):
		if len(seg) == 8 && !is(7, "replies") {
			return ""
		}
		if team, channel := id(2), id(4); team != "" && channel != "" {
			return team + "/" + channel
		}
	}
	return ""
}
