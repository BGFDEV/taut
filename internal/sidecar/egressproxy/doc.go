// Package egressproxy implements the egress proxy: the forward proxy every
// worker request to Microsoft or Slack goes through. It adds the token and
// enforces the rate limits.
//
// A worker holds no credentials and opens no outbound connections. It
// sends an ordinary HTTP request to the egress proxy on localhost, with two
// extra headers (see package wire):
//
//	X-Taut-Upstream: https://graph.microsoft.com      the real origin
//	X-Taut-Auth:     base64url(JSON wire.Auth)        how to authenticate
//
// The egress proxy checks the origin against its allowlist, waits until the
// request fits the tenant's and the channel's rate budget, asks the token
// broker for a bearer token, and forwards the request over TLS to
// {upstream}{path}?{query}. Status, headers and body come back unchanged.
// Bodies are streamed in both directions and never buffered, except request
// bodies of at most 1 MiB on authenticated requests, which are kept so the
// request can be replayed once after a 401.
//
// # Listening address
//
// The egress proxy has no authentication of its own: whoever can reach it can
// make authenticated calls as the migration. It MUST listen on a loopback
// address (127.0.0.1 or ::1) inside the pod's network namespace. The address
// is the caller's choice; Serve logs a warning for any other.
//
// # It is not an open proxy
//
// Only https origins on the allowlist are reachable (DefaultAllowHosts plus
// Config.AllowHosts); IP literals, other ports and plain http are refused
// unless an entry names them explicitly. Redirects are never followed: a 3xx
// is handed back to the worker, whose next request is checked like any
// other, so a redirect cannot carry a token off the allowlist.
//
// A credential is attached only where it belongs, decided before the token
// broker is asked for anything: an Entra token only to the Graph origin its
// own descriptor resolves to (graphBase, else the cloud's Graph base), on a
// host classified graph, never to SharePoint, whose upload sessions are
// pre-authorized, nor to another cloud's Graph; a Slack token only to the
// Slack API under /api/, or to a file host for GET and HEAD; and hosts the
// operator added without a scope get no credential at all. Inbound
// Authorization and X-Taut-* headers are dropped.
//
// Should an upstream echo the bearer token in a response header or trailer,
// that header is removed before the response reaches the worker. Response
// bodies are not inspected: they are streamed through in constant memory,
// and a token could straddle any two chunks, so scrubbing them would mean
// buffering. An upstream that echoed the Authorization header in a body
// would therefore defeat the "workers never see a token" property; none of
// Graph, SharePoint or Slack does.
//
// # Admission
//
// The egress proxy delays; it never rejects for rate. Each request passes:
//
//   - a tenant bucket per (tenant, scope), whose rate is the lease from the
//     control plane times an adaptive multiplier in [0.1, 1]. The lease is
//     refreshed in the background at TTL/2 while the key has traffic. If it
//     cannot be obtained, or has expired, the rate is FallbackRPS (capped by
//     the scope's default total and by the last lease), never unlimited;
//   - for Teams channel message posts, a channel bucket per team/channel,
//     starting at ChannelStartRPS and adapting up to ChannelMaxRPS. It is
//     local on purpose: a channel is imported by exactly one worker at a
//     time, so no other egress proxy sends to it;
//   - a pause gate on each, set by throttling responses.
//
// A bucket holds at most one second of its rate (at least one request). A
// request canceled while waiting consumes nothing.
//
// # Feedback
//
// The egress proxy reacts to each response and then passes it on unchanged,
// including a 429. It does not retry: the worker's HTTP client already
// honors Retry-After, and its retry re-enters admission, by which
// time the pause gate is holding everyone back. The single exception is a
// 401 on an authenticated request with no body or a small buffered one: the
// token broker is asked for a fresh token and the request is sent once more.
//
// On 429, or 503 with Retry-After (a 429 without one counts as 5 s):
//
//   - Request without a channel key: the tenant scope pauses for Retry-After
//     plus up to 10 % jitter, and its multiplier is halved (floor 0.1).
//   - Request with a channel key: Graph does not say whether the per-channel
//     or the per-tenant limit was hit, and the per-channel limit is by far
//     the more likely one for a message post. So the channel takes the full
//     consequence (paused for Retry-After plus jitter, rate halved, floor
//     0.1 rps) while the tenant scope gets a hedge: paused for
//     min(Retry-After, 2 s) and its multiplier cut by 10 %. If the tenant
//     limit really was the cause, other requests will draw 429s of their own
//     and repeated hedges compound quickly; if it was not, one hot channel
//     has cost the rest of the migration two seconds, not a minute.
//   - Decreases are applied once per congestion event: a throttling response
//     to a request that was admitted before the last decrease extends the
//     pause but does not cut the rate again, so a burst of in-flight
//     requests failing together counts once.
//   - The tenant-scope pause is published on the bus (subject taut.throttle).
//     Every egress proxy applies pauses published by others for tenants it is
//     serving, which is how all egress proxies of a tenant back off together.
//
// On success (2xx or 3xx) the tenant multiplier grows by 0.02 per second of
// successful traffic back toward 1 (from a halving, full rate returns after
// about 25 s of clean traffic) and a channel's rate grows by 0.5 rps per 30
// successful posts up to ChannelMaxRPS. Other 4xx and 5xx responses change
// nothing.
//
// # The bus is not authenticated
//
// Leases and throttle events arrive over NATS, which by itself lets any
// client publish on any subject. The real control is NATS subject
// permissions, which the deployment must set: workers may use the wake
// subject only; egress proxies may publish taut.throttle and request
// taut.budget.lease; only the control plane may answer leases. The egress proxy
// additionally bounds what a forged message can do, but only bounds it:
//
//   - a lease is clamped to the scope's whole budget (budget.DefaultTotals,
//     or Config.MaxRPS when the operator raised a total), must be finite and
//     positive, and is believed for at most 60 s;
//   - a remote throttle event pauses for at most 30 s (a 429 the egress proxy
//     saw itself keeps the 10-minute cap), is ignored unless its timestamp
//     is within 60 s of now, never changes a rate, and never creates state.
//
// Tenant buckets are keyed by a string the caller supplies, so their number
// is capped (Config.MaxTenants); keys beyond the cap share one overflow
// bucket at the fallback rate and request no leases.
//
// # Secrets
//
// Tokens exist only in the Authorization header of the outgoing request.
// They are never logged, never part of an error and never in /_proxy/state.
// URLs are logged through secret.Redact, which removes Slack's ?t= tokens.
package egressproxy
