package graph

import (
	"context"
	"iter"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// Organization is the tenant's directory record.
type Organization struct {
	ID              string           `json:"id"`
	DisplayName     string           `json:"displayName"`
	VerifiedDomains []VerifiedDomain `json:"verifiedDomains"`
}

// VerifiedDomain is a domain the tenant has proven it owns.
type VerifiedDomain struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	IsInitial bool   `json:"isInitial"`
}

// User is an Entra ID user. Fields not requested with UserQuery.Select are
// left at their zero value.
type User struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
	// AccountEnabled is nil when the field was not selected.
	AccountEnabled *bool    `json:"accountEnabled"`
	UserType       string   `json:"userType"`
	ProxyAddresses []string `json:"proxyAddresses"`
}

// Disabled reports whether the account is known to be disabled.
func (u User) Disabled() bool { return u.AccountEnabled != nil && !*u.AccountEnabled }

// Team is a Microsoft Teams team.
type Team struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	// Visibility is "private" or "public". It is filled by GetTeam only.
	Visibility string `json:"visibility"`
}

// Channel is a channel in a team.
type Channel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	// MembershipType is "standard", "private" or "shared".
	MembershipType string `json:"membershipType"`
	// CreatedDateTime is when the channel was created or, after a
	// startMigration with a backdated time, that time. Imported messages
	// must be later than it.
	CreatedDateTime time.Time `json:"createdDateTime"`
	// MigrationMode is "" (never migrated), MigrationInProgress or
	// MigrationCompleted.
	MigrationMode string `json:"migrationMode"`
	// OriginalCreatedDateTime is the creation time before migration mode
	// changed it; nil if the channel never entered migration mode.
	OriginalCreatedDateTime *time.Time `json:"originalCreatedDateTime"`
}

// Channel.MigrationMode values.
const (
	MigrationInProgress = "inProgress"
	MigrationCompleted  = "completed"
)

// DefaultUserSelect lists the user fields Taut reads. Graph omits
// accountEnabled, userType and proxyAddresses unless they are selected.
func DefaultUserSelect() []string {
	return []string{"id", "displayName", "mail", "userPrincipalName", "accountEnabled", "userType", "proxyAddresses"}
}

// UserQuery narrows a Users listing.
type UserQuery struct {
	// Select lists the fields to return; empty means DefaultUserSelect.
	Select []string
	// Filter is an OData $filter expression; build simple ones with FilterEq.
	Filter string
	// PageSize is $top (1–999); zero lets Graph choose (100).
	PageSize int
}

// FilterEq builds the OData expression `field eq 'value'`. A single quote
// in value is escaped by doubling it, as OData requires.
func FilterEq(field, value string) string {
	return field + " eq '" + strings.ReplaceAll(value, "'", "''") + "'"
}

// The application permissions each read endpoint needs, used to make 403
// errors actionable.
const (
	permOrganization = "Organization.Read.All"
	permUsers        = "User.Read.All"
	permTeams        = "Team.ReadBasic.All"
	permChannels     = "Channel.ReadBasic.All"
)

// Organization returns the tenant's organization record.
func (c *Client) Organization(ctx context.Context) (Organization, error) {
	var p page[Organization]
	rq := request{
		method: "GET", path: "/v1.0/organization", permission: permOrganization,
		query: url.Values{"$select": {"id,displayName,verifiedDomains"}},
	}
	if err := c.do(ctx, rq, &p); err != nil {
		return Organization{}, err
	}
	if len(p.Value) == 0 {
		return Organization{}, fail.New("graph.bad_response",
			"Microsoft Graph returned no organization for this tenant",
			"GET /v1.0/organization answered with an empty list.",
			"Check that the tenant ID is correct and run `taut auth` again.")
	}
	return p.Value[0], nil
}

// Users iterates the tenant's users, following paging transparently.
func (c *Client) Users(ctx context.Context, q UserQuery) iter.Seq2[User, error] {
	query := url.Values{"$select": {strings.Join(userSelect(q.Select), ",")}}
	if q.Filter != "" {
		query.Set("$filter", q.Filter)
	}
	if q.PageSize > 0 {
		query.Set("$top", strconv.Itoa(q.PageSize))
	}
	return list[User](ctx, c, request{method: "GET", path: "/v1.0/users", query: query, permission: permUsers})
}

// UserByID fetches one user by object ID or user principal name. When no
// such user exists the error matches ErrNotFound.
func (c *Client) UserByID(ctx context.Context, idOrUPN string) (User, error) {
	if strings.TrimSpace(idOrUPN) == "" {
		return User{}, fail.New("graph.bad_request", "No user was specified",
			"The user ID or user principal name is empty.",
			"Pass an Entra object ID or a UPN such as ada@contoso.com.").WithExit(fail.ExitUsage)
	}
	var u User
	rq := request{
		method: "GET", path: "/v1.0/users/" + url.PathEscape(idOrUPN), permission: permUsers,
		query: url.Values{"$select": {strings.Join(DefaultUserSelect(), ",")}},
	}
	return u, c.do(ctx, rq, &u)
}

// UserByUPN is UserByID; Graph accepts either key in the same position.
func (c *Client) UserByUPN(ctx context.Context, upn string) (User, error) {
	return c.UserByID(ctx, upn)
}

// Teams iterates every team in the tenant.
func (c *Client) Teams(ctx context.Context) iter.Seq2[Team, error] {
	return list[Team](ctx, c, request{
		method: "GET", path: "/v1.0/teams", permission: permTeams,
		query: url.Values{"$select": {"id,displayName,description"}},
	})
}

// Channels iterates the channels of one team. An unknown team yields an
// error matching ErrNotFound.
func (c *Client) Channels(ctx context.Context, teamID string) iter.Seq2[Channel, error] {
	return list[Channel](ctx, c, request{
		method: "GET", path: "/v1.0/teams/" + url.PathEscape(teamID) + "/channels", permission: permChannels,
	})
}

func userSelect(sel []string) []string {
	if len(sel) == 0 {
		return DefaultUserSelect()
	}
	return sel
}
