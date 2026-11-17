package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

// collect drains an iterator, stopping at the first error.
func collect[T any](seq func(func(T, error) bool)) ([]T, error) {
	var out []T
	for v, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, nil
}

func manyUsers(n int) fakegraph.Seed {
	seed := fakegraph.DefaultSeed()
	seed.Users = nil
	for i := range n {
		seed.Users = append(seed.Users, fakegraph.User{
			ID:                fmt.Sprintf("u-%04d", i),
			DisplayName:       fmt.Sprintf("User %04d", i),
			Mail:              fmt.Sprintf("user%04d@contoso.example", i),
			UserPrincipalName: fmt.Sprintf("user%04d@contoso.example", i),
			AccountEnabled:    i%10 != 0,
		})
	}
	return seed
}

func TestExpiredTokenIsRefreshedTransparently(t *testing.T) {
	// The server's tokens die after two minutes, but the client believes
	// its cached copy is good: only the server's clock moves.
	serverClock := newClock()
	e := newEnv(t, withServer(fakegraph.WithClock(serverClock.Now), fakegraph.WithTokenLifetime(10*time.Minute)))
	ctx := context.Background()
	if _, err := e.client.Organization(ctx); err != nil {
		t.Fatal(err)
	}
	serverClock.Advance(11 * time.Minute)
	if _, err := e.client.Organization(ctx); err != nil {
		t.Fatalf("Organization after server-side expiry: %v", err)
	}

	var statuses []int
	for _, r := range e.fake.Requests() {
		if r.Path == "/v1.0/organization" {
			statuses = append(statuses, r.Status)
		}
	}
	if fmt.Sprint(statuses) != "[200 401 200]" {
		t.Errorf("organization statuses = %v, want [200 401 200]", statuses)
	}
	if got := e.fake.CountRequests("POST", tokenPath); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
	if len(e.sleep.Waits()) != 0 {
		t.Errorf("a 401 refresh should not sleep: %v", e.sleep.Waits())
	}
}

func TestPersistent401IsReportedOnce(t *testing.T) {
	e := newEnv(t)
	e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/**", Action: fakegraph.ActionStatus, Status: 401})
	_, err := e.client.Organization(context.Background())
	wantFail(t, err, "graph.unauthorized")
	if got := e.fake.CountRequests("GET", "/v1.0/organization"); got != 2 {
		t.Errorf("requests = %d, want 2 (original + one refresh)", got)
	}
}

func TestMissingRoleIsAPermissionError(t *testing.T) {
	e := newEnv(t, withRoles("Teamwork.Migrate.All", "Organization.Read.All"))
	ctx := context.Background()

	_, err := collect(e.client.Users(ctx, graph.UserQuery{}))
	fe := wantFail(t, err, "graph.forbidden")
	if fail.ExitCode(err) != fail.ExitPermission {
		t.Errorf("exit code = %d, want %d", fail.ExitCode(err), fail.ExitPermission)
	}
	if !strings.Contains(fe.Next, "User.Read.All") || !strings.Contains(fe.Next, "taut auth") {
		t.Errorf("Next should name the permission and the command: %q", fe.Next)
	}
	var api *graph.APIError
	if !errors.As(err, &api) {
		t.Fatalf("no APIError in chain: %v", err)
	}
	if api.Status != 403 || api.Code != "Authorization_RequestDenied" || api.RequestID == "" || api.Date == "" ||
		api.Method != "GET" || api.Path != "/v1.0/users" || api.Attempts != 1 {
		t.Errorf("APIError = %+v", api)
	}
	if !strings.Contains(fe.Why, api.RequestID) {
		t.Errorf("Why should carry the request-id: %q", fe.Why)
	}

	// The same token still works where its roles allow.
	if _, err := e.client.Organization(ctx); err != nil {
		t.Errorf("Organization: %v", err)
	}
	// And the comparison table explains the gap without another call.
	token, _ := e.tokens.Token(ctx)
	roles, _ := graph.Roles(token)
	missing := graph.MissingRequired(graph.Compare(roles, graph.FeatureSet{}))
	if len(missing) != 7 || missing[0] != "User.Read.All" {
		t.Errorf("missing = %v", missing)
	}

	for _, tc := range []struct {
		call func() error
		perm string
	}{
		{func() error { _, err := collect(e.client.Teams(ctx)); return err }, "Team.ReadBasic.All"},
		{func() error { _, err := collect(e.client.Channels(ctx, "x")); return err }, "Channel.ReadBasic.All"},
		{func() error { _, err := e.client.UserByID(ctx, "x"); return err }, "User.Read.All"},
	} {
		fe := wantFail(t, tc.call(), "graph.forbidden")
		if !strings.Contains(fe.Next, tc.perm) || fe.Exit != fail.ExitPermission {
			t.Errorf("want %s in Next with exit 4: %+v", tc.perm, fe)
		}
	}
}

func TestUsersPaging(t *testing.T) {
	const n = 1234
	e := newEnv(t, withSeed(manyUsers(n)))
	ctx := context.Background()

	users, err := collect(e.client.Users(ctx, graph.UserQuery{PageSize: 100}))
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != n {
		t.Fatalf("got %d users, want %d", len(users), n)
	}
	for i, u := range users {
		if u.ID != fmt.Sprintf("u-%04d", i) {
			t.Fatalf("users[%d].ID = %s: order or duplicates are wrong", i, u.ID)
		}
	}
	u := users[10]
	if u.Mail != "user0010@contoso.example" || u.UserType != "Member" || u.AccountEnabled == nil || *u.AccountEnabled ||
		!u.Disabled() || len(u.ProxyAddresses) != 1 || u.ProxyAddresses[0] != "SMTP:user0010@contoso.example" {
		t.Errorf("user fields not populated by default select: %+v", u)
	}
	if got := e.fake.CountRequests("GET", "/v1.0/users"); got != 13 {
		t.Errorf("page requests = %d, want 13", got)
	}

	// Server default page size, and a narrower select.
	e.fake.ResetRequests()
	users, err = collect(e.client.Users(ctx, graph.UserQuery{Select: []string{"id", "mail"}}))
	if err != nil || len(users) != n {
		t.Fatalf("default paging: %d users, err %v", len(users), err)
	}
	if users[0].DisplayName != "" || users[0].AccountEnabled != nil || users[0].Disabled() || users[0].Mail == "" {
		t.Errorf("select not honored: %+v", users[0])
	}
	if got := e.fake.CountRequests("GET", "/v1.0/users"); got != 13 {
		t.Errorf("page requests = %d, want 13", got)
	}

	// Stopping early stops fetching.
	e.fake.ResetRequests()
	seen := 0
	for _, err := range e.client.Users(ctx, graph.UserQuery{PageSize: 50}) {
		if err != nil {
			t.Fatal(err)
		}
		if seen++; seen == 60 {
			break
		}
	}
	if got := e.fake.CountRequests("GET", "/v1.0/users"); got != 2 {
		t.Errorf("page requests after early break = %d, want 2", got)
	}
}

func TestPagingSurfacesMidStreamErrors(t *testing.T) {
	e := newEnv(t, withSeed(manyUsers(30)), withRetry(graph.RetryPolicy{MaxAttempts: 2}))
	n, calls := 0, 0
	var gotErr error
	for _, err := range e.client.Users(context.Background(), graph.UserQuery{PageSize: 10}) {
		calls++
		if err != nil {
			gotErr = err
			continue // the iterator must stop by itself after an error
		}
		if n++; n == 10 {
			e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/users", Action: fakegraph.ActionStatus, Status: 503})
		}
	}
	wantFail(t, gotErr, "graph.unavailable")
	if n != 10 || calls != 11 {
		t.Errorf("yielded %d users in %d calls, want 10 in 11", n, calls)
	}
}

func TestUserFilters(t *testing.T) {
	seed := fakegraph.DefaultSeed()
	seed.Users = append(seed.Users, fakegraph.User{
		ID: "u-obrien", DisplayName: "Miles O'Brien", Mail: "miles.o'brien@contoso.example",
		UserPrincipalName: "miles.o'brien@contoso.example", AccountEnabled: true,
	})
	e := newEnv(t, withSeed(seed))
	ctx := context.Background()

	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{"mail", graph.FilterEq("mail", "grace@contoso.example"), []string{"Grace Hopper"}},
		{"mail is case-insensitive", graph.FilterEq("mail", "GRACE@Contoso.Example"), []string{"Grace Hopper"}},
		{"upn", graph.FilterEq("userPrincipalName", "alan@contoso.example"), []string{"Alan Turing"}},
		{"apostrophe is escaped", graph.FilterEq("mail", "miles.o'brien@contoso.example"), []string{"Miles O'Brien"}},
		{"value containing and", graph.FilterEq("displayName", "Linus (Fabrikam)") + " and userType eq 'Guest'", []string{"Linus (Fabrikam)"}},
		{"disabled", "accountEnabled eq false", []string{"Edsger Dijkstra"}},
		{"conjunction", "accountEnabled eq true and userType eq 'Guest'", []string{"Linus (Fabrikam)"}},
		{"no match", graph.FilterEq("mail", "nobody@contoso.example"), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			users, err := collect(e.client.Users(ctx, graph.UserQuery{Filter: tc.filter}))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, u := range users {
				names = append(names, u.DisplayName)
			}
			if fmt.Sprint(names) != fmt.Sprint(tc.want) {
				t.Errorf("filter %q → %v, want %v", tc.filter, names, tc.want)
			}
		})
	}

	for _, bad := range []string{"startswith(mail,'a')", "jobTitle eq 'x'", "accountEnabled eq 'yes'", "mail eq grace"} {
		_, err := collect(e.client.Users(ctx, graph.UserQuery{Filter: bad}))
		fe := wantFail(t, err, "graph.bad_request")
		var api *graph.APIError
		if !errors.As(fe, &api) || api.Status != 400 {
			t.Errorf("filter %q: %+v", bad, api)
		}
	}
	if len(e.sleep.Waits()) != 0 {
		t.Errorf("400s must not be retried: %v", e.sleep.Waits())
	}
}

func TestUserByID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	u, err := e.client.UserByID(ctx, "a0000000-0000-4000-8000-000000000002")
	if err != nil || u.DisplayName != "Grace Hopper" || u.AccountEnabled == nil || !*u.AccountEnabled {
		t.Fatalf("by id: %+v, %v", u, err)
	}
	// Guest UPNs contain '#', which must be escaped in the path.
	const guest = "linus_fabrikam.example#EXT#@contoso.onmicrosoft.example"
	u, err = e.client.UserByUPN(ctx, guest)
	if err != nil || u.UserType != "Guest" || u.UserPrincipalName != guest || u.Mail != "linus@fabrikam.example" {
		t.Fatalf("by guest upn: %+v, %v", u, err)
	}
	// A user without a mailbox decodes cleanly.
	u, err = e.client.UserByUPN(ctx, "svc-taut@contoso.example")
	if err != nil || u.Mail != "" || len(u.ProxyAddresses) != 0 {
		t.Fatalf("mailless user: %+v, %v", u, err)
	}

	_, err = e.client.UserByID(ctx, "ghost@contoso.example")
	fe := wantFail(t, err, "graph.not_found")
	if !graph.IsNotFound(err) || !errors.Is(fe, graph.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	var api *graph.APIError
	if !errors.As(err, &api) || api.Code != "Request_ResourceNotFound" {
		t.Errorf("APIError = %+v", api)
	}
	if got := e.fake.CountRequests("GET", "/v1.0/users/*"); got != 4 {
		t.Errorf("requests = %d, want 4 (404 is not retried)", got)
	}
	_, err = e.client.UserByID(ctx, " ")
	wantFail(t, err, "graph.bad_request")
}

func TestTeamsAndChannels(t *testing.T) {
	seed := fakegraph.DefaultSeed()
	for i := range 25 {
		seed.Teams = append(seed.Teams, fakegraph.Team{ID: fmt.Sprintf("t-%02d", i), DisplayName: fmt.Sprintf("Team %02d", i)})
	}
	e := newEnv(t, withSeed(seed), withServer(fakegraph.WithPageSize(10)))
	ctx := context.Background()

	teams, err := collect(e.client.Teams(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 27 || teams[0].DisplayName != "Engineering" || teams[0].Description != "Existing engineering team" || teams[26].ID != "t-24" {
		t.Fatalf("teams = %d, first %+v", len(teams), teams[0])
	}
	if got := e.fake.CountRequests("GET", "/v1.0/teams"); got != 3 {
		t.Errorf("team pages = %d, want 3", got)
	}

	channels, err := collect(e.client.Channels(ctx, teams[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 3 || channels[0].DisplayName != "General" || channels[2].MembershipType != "private" ||
		channels[0].ID != "19:general-eng@thread.tacv2" {
		t.Errorf("channels = %+v", channels)
	}

	_, err = collect(e.client.Channels(ctx, "no-such-team"))
	wantFail(t, err, "graph.not_found")
	if !graph.IsNotFound(err) {
		t.Errorf("want not found: %v", err)
	}
}

func TestThrottleHonorsRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.fake.Faults().MustAdd(fakegraph.Fault{
		Method: "GET", Path: "/v1.0/organization", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 7, Count: 2,
	})
	if _, err := e.client.Organization(context.Background()); err != nil {
		t.Fatalf("Organization: %v", err)
	}
	waits := e.sleep.Waits()
	if len(waits) != 2 {
		t.Fatalf("sleeps = %v, want 2", waits)
	}
	for _, w := range waits {
		if w < 7*time.Second || w > 8*time.Second {
			t.Errorf("slept %v; want at least Retry-After (7s) plus bounded jitter", w)
		}
	}
	if got := e.fake.CountRequests("GET", "/v1.0/organization"); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"12", 12 * time.Second, true},
		{" 0 ", 0, true},
		{"1.5", 1500 * time.Millisecond, true},
		{"-3", 0, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{"soon", 0, false},
	} {
		got, ok := graph.ParseRetryAfterForTest(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestServerRateLimitIsRiddenOut(t *testing.T) {
	// One request per second with a burst of two; the fake sleeper advances
	// the shared clock, which refills the bucket.
	e := newEnv(t, withServer(fakegraph.WithRateLimit(1, 2)))
	ctx := context.Background()
	for i := range 6 {
		if _, err := e.client.Organization(ctx); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	waits := e.sleep.Waits()
	if len(waits) != 4 {
		t.Fatalf("sleeps = %v, want 4 (two burst calls are free)", waits)
	}
	for _, w := range waits {
		if w < time.Second {
			t.Errorf("slept %v, less than the server's Retry-After", w)
		}
	}
	throttled := 0
	for _, r := range e.fake.Requests() {
		if r.Status == 429 {
			throttled++
			if r.AppID != fakegraph.DefaultClientID {
				t.Errorf("429 not attributed to the app: %+v", r)
			}
		}
	}
	if throttled != 4 {
		t.Errorf("429s = %d, want 4", throttled)
	}
}

func TestServiceUnavailableIsRetried(t *testing.T) {
	for _, status := range []int{503, 504} {
		e := newEnv(t)
		e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/users", Action: fakegraph.ActionStatus, Status: status, Count: 2})
		users, err := collect(e.client.Users(context.Background(), graph.UserQuery{}))
		if err != nil || len(users) != 7 {
			t.Fatalf("%d: users = %d, err = %v", status, len(users), err)
		}
		// Jitter is pinned to 0.5: half of 500ms, then half of 1s.
		if got := fmt.Sprint(e.sleep.Waits()); got != "[250ms 500ms]" {
			t.Errorf("%d: backoff = %s, want [250ms 500ms]", status, got)
		}
	}
}

func TestDroppedConnectionIsRetried(t *testing.T) {
	e := newEnv(t)
	id := e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/**", Action: fakegraph.ActionDrop, Count: 1})
	if _, err := e.client.Organization(context.Background()); err != nil {
		t.Fatalf("Organization: %v", err)
	}
	reqs := e.fake.Requests()
	last := reqs[len(reqs)-2:]
	if last[0].Status != 0 || last[0].Fault != id || last[1].Status != 200 {
		t.Errorf("log tail = %+v", last)
	}

	e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/**", Action: fakegraph.ActionFailAfterCommit})
	_, err := e.client.Organization(context.Background())
	fe := wantFail(t, err, "graph.network")
	var api *graph.APIError
	if !errors.As(fe, &api) || api.Status != 0 || api.Attempts != 5 || api.Err == nil {
		t.Errorf("APIError = %+v", api)
	}
}

func TestRetryLimitExhausted(t *testing.T) {
	e := newEnv(t, withRetry(graph.RetryPolicy{MaxAttempts: 4}))
	e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/organization", Action: fakegraph.ActionStatus, Status: 503})
	_, err := e.client.Organization(context.Background())
	fe := wantFail(t, err, "graph.unavailable")
	var api *graph.APIError
	if !errors.As(err, &api) || api.Status != 503 || api.Attempts != 4 || api.Code != "serviceNotAvailable" {
		t.Errorf("APIError = %+v", api)
	}
	if !strings.Contains(fe.Why, "4 attempt") {
		t.Errorf("Why should say how hard we tried: %q", fe.Why)
	}
	if got := e.fake.CountRequests("GET", "/v1.0/organization"); got != 4 {
		t.Errorf("requests = %d, want 4", got)
	}
	if got := len(e.sleep.Waits()); got != 3 {
		t.Errorf("sleeps = %d, want 3", got)
	}
	if fail.ExitCode(err) != fail.ExitFailure {
		t.Errorf("exit = %d", fail.ExitCode(err))
	}
}

func TestWaitBudgetExhausted(t *testing.T) {
	e := newEnv(t, withRetry(graph.RetryPolicy{MaxAttempts: 10, MaxTotalWait: 25 * time.Second}))
	e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/organization", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 10})
	_, err := e.client.Organization(context.Background())
	fe := wantFail(t, err, "graph.throttled")
	var api *graph.APIError
	if !errors.As(err, &api) || api.Status != 429 || api.RetryAfter != 10*time.Second || api.Attempts != 3 {
		t.Errorf("APIError = %+v", api)
	}
	if !strings.Contains(fe.Why, "wait budget") {
		t.Errorf("Why = %q", fe.Why)
	}
	var total time.Duration
	for _, w := range e.sleep.Waits() {
		total += w
	}
	if len(e.sleep.Waits()) != 2 || total > 25*time.Second {
		t.Errorf("waits = %v (total %v), want two within the 25s budget", e.sleep.Waits(), total)
	}

	// A single Retry-After beyond the whole budget fails without sleeping.
	e2 := newEnv(t, withRetry(graph.RetryPolicy{MaxTotalWait: time.Minute}))
	e2.fake.Faults().MustAdd(fakegraph.Fault{Action: fakegraph.ActionThrottle, Path: "/v1.0/**", RetryAfterSeconds: 3600})
	_, err = e2.client.Organization(context.Background())
	wantFail(t, err, "graph.throttled")
	if len(e2.sleep.Waits()) != 0 {
		t.Errorf("slept %v for a Retry-After that exceeds the budget", e2.sleep.Waits())
	}
}

func TestContextCancelledDuringBackoff(t *testing.T) {
	e := newEnv(t)
	e.fake.Faults().MustAdd(fakegraph.Fault{Path: "/v1.0/**", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 30})

	ctx, cancel := context.WithCancel(context.Background())
	var asked atomic.Int64
	client, err := graph.New(graph.Options{
		BaseURL: e.http.URL, HTTPClient: e.http.Client(), TokenSource: e.tokens,
		Sleep: func(ctx context.Context, d time.Duration) error {
			asked.Store(int64(d))
			cancel() // the operator presses Ctrl-C mid-wait
			return graph.SleepCtxForTest(ctx, d)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.Organization(ctx)
	fe := wantFail(t, err, "graph.canceled")
	if !errors.Is(fe, context.Canceled) {
		t.Errorf("want context.Canceled in chain: %v", err)
	}
	if time.Duration(asked.Load()) < 30*time.Second {
		t.Errorf("sleeper asked for %v", time.Duration(asked.Load()))
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancellation did not interrupt the backoff")
	}
	if got := e.fake.CountRequests("GET", "/v1.0/organization"); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}

	// A context that is already done never reaches the network.
	e.fake.ResetRequests()
	_, err = e.client.Organization(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	wantFail(t, err, "graph.canceled")
	if got := len(e.fake.Requests()); got != 0 {
		t.Errorf("requests with a dead context = %d", got)
	}
}

func TestCountLimitedAndLatencyFaults(t *testing.T) {
	e := newEnv(t, withRetry(graph.RetryPolicy{MaxAttempts: 1}))
	ctx := context.Background()
	f := e.fake.Faults()
	f.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/teams/*/channels", Action: fakegraph.ActionStatus, Status: 500, Count: 2})
	f.MustAdd(fakegraph.Fault{Path: "/v1.0/teams", Action: fakegraph.ActionLatency, LatencyMS: 1500, Count: 1})

	const team = "b0000000-0000-4000-8000-000000000001"
	for i := range 2 {
		_, err := collect(e.client.Channels(ctx, team))
		wantFail(t, err, "graph.unavailable")
		if i == 0 {
			// The rule matches only its own path.
			if _, err := collect(e.client.Teams(ctx)); err != nil {
				t.Fatalf("Teams: %v", err)
			}
		}
	}
	if _, err := collect(e.client.Channels(ctx, team)); err != nil {
		t.Fatalf("third Channels call should pass once the rule is spent: %v", err)
	}
	if got := fmt.Sprint(e.sleep.Waits()); got != "[1.5s]" {
		t.Errorf("latency waits = %s, want [1.5s]", got)
	}
	list := f.List()
	if len(list) != 2 || list[0].Fired != 2 || !list[0].Exhausted || list[1].Fired != 1 || !list[1].Exhausted {
		t.Errorf("fault status = %+v", list)
	}
}

func TestSecretsNeverAppearInErrorsOrLogs(t *testing.T) {
	e := newEnv(t, withRoles("Organization.Read.All"), withRetry(graph.RetryPolicy{MaxAttempts: 2}))
	ctx := context.Background()
	token, err := e.tokens.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sig := token[strings.LastIndex(token, ".")+1:]
	secrets := []string{token, sig, fakegraph.DefaultSecret}

	var errs []error
	add := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		errs = append(errs, err)
	}
	_, err = collect(e.client.Users(ctx, graph.UserQuery{})) // 403
	add(err)
	f := e.fake.Faults()
	for _, fault := range []fakegraph.Fault{
		{Path: "/v1.0/**", Action: fakegraph.ActionStatus, Status: 401},
		{Path: "/v1.0/**", Action: fakegraph.ActionStatus, Status: 400},
		{Path: "/v1.0/**", Action: fakegraph.ActionStatus, Status: 404},
		{Path: "/v1.0/**", Action: fakegraph.ActionStatus, Status: 418},
		{Path: "/v1.0/**", Action: fakegraph.ActionStatus, Status: 503},
		{Path: "/v1.0/**", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1},
		{Path: "/v1.0/**", Action: fakegraph.ActionDrop},
	} {
		f.Clear()
		f.MustAdd(fault)
		_, err = e.client.Organization(ctx)
		add(err)
	}
	f.Clear()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = e.client.Organization(cctx)
	add(err)

	codes := map[string]bool{}
	for _, err := range errs {
		assertNoSecrets(t, err, secrets...)
		codes[fail.From(err).Code] = true
	}
	for _, want := range []string{"graph.forbidden", "graph.unauthorized", "graph.bad_request", "graph.not_found",
		"graph.request_failed", "graph.unavailable", "graph.throttled", "graph.network", "graph.canceled"} {
		if !codes[want] {
			t.Errorf("scenario for %s did not produce that code; got %v", want, codes)
		}
	}

	// The fake's own log and state dump are secret-free too.
	logJSON, _ := json.Marshal(e.fake.Requests())
	stateJSON, _ := json.Marshal(e.fake.Snapshot())
	for _, s := range secrets {
		if strings.Contains(string(logJSON), s) || strings.Contains(string(stateJSON), s) {
			t.Errorf("fake server log or state leaks %q...", s[:8])
		}
	}
	if !strings.Contains(string(logJSON), fakegraph.DefaultClientID) {
		t.Error("request log should attribute calls to the app ID")
	}
}

func TestNextLinkToAnotherHostIsRefused(t *testing.T) {
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { evilHits.Add(1) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer static-token" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("User-Agent") != "taut/test" || r.Header.Get("client-request-id") == "" {
			t.Errorf("headers = %v", r.Header)
		}
		fmt.Fprintf(w, `{"value":[{"id":"t1","displayName":"One"}],"@odata.nextLink":%q}`, evil.URL+"/v1.0/teams?$skiptoken=x")
	}))
	defer srv.Close()

	c, err := graph.New(graph.Options{BaseURL: srv.URL + "/", TokenSource: staticToken("static-token"), UserAgent: "taut/test"})
	if err != nil {
		t.Fatal(err)
	}
	teams, err := collect(c.Teams(context.Background()))
	wantFail(t, err, "graph.bad_response")
	if len(teams) != 1 {
		t.Errorf("first page should still be delivered: %v", teams)
	}
	if evilHits.Load() != 0 {
		t.Error("the bearer token was sent to a foreign host")
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func TestQueryEncodingAndBadResponses(t *testing.T) {
	var rawQuery atomic.Value
	mode := "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery.Store(r.URL.RawQuery)
		switch mode {
		case "html":
			fmt.Fprint(w, "<html>captive portal</html>")
		case "plain500":
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
		case "empty":
			fmt.Fprint(w, `{"value":[]}`)
		default:
			fmt.Fprint(w, `{"value":[]}`)
		}
	}))
	defer srv.Close()
	c, err := graph.New(graph.Options{BaseURL: srv.URL, TokenSource: staticToken("t"), Retry: graph.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := collect(c.Users(ctx, graph.UserQuery{Filter: "mail eq 'a b+c@x.example'", PageSize: 5})); err != nil {
		t.Fatal(err)
	}
	q := rawQuery.Load().(string)
	if strings.Contains(q, "+") || !strings.Contains(q, "mail%20eq%20%27a%20b%2Bc%40x.example%27") || !strings.Contains(q, "%24top=5") {
		t.Errorf("raw query = %s", q)
	}

	mode = "empty"
	_, err = c.Organization(ctx)
	wantFail(t, err, "graph.bad_response")
	mode = "html"
	_, err = c.Organization(ctx)
	wantFail(t, err, "graph.bad_response")
	mode = "plain500"
	_, err = c.Organization(ctx)
	fe := wantFail(t, err, "graph.unavailable") // non-JSON error bodies still map by status
	if !strings.Contains(fe.Why, "500") {
		t.Errorf("Why = %q", fe.Why)
	}

	_, err = graph.New(graph.Options{BaseURL: "ftp://x", TokenSource: staticToken("t")})
	wantFail(t, err, "graph.config_invalid")
	_, err = graph.New(graph.Options{})
	wantFail(t, err, "graph.config_invalid")
}

func TestPagesHelper(t *testing.T) {
	e := newEnv(t, withSeed(manyUsers(25)))
	type slim struct {
		ID string `json:"id"`
	}
	got, err := collect(graph.Pages[slim](context.Background(), e.client, "/v1.0/users", map[string][]string{"$top": {"10"}, "$select": {"id"}}))
	if err != nil || len(got) != 25 || got[24].ID != "u-0024" {
		t.Fatalf("Pages: %d items, err %v", len(got), err)
	}
}
