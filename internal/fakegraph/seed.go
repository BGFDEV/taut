package fakegraph

import (
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // x5t is defined as the SHA-1 thumbprint
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Seed is the initial content of a fake tenant. It is plain data and
// round-trips through JSON, so it can live in a file (see LoadSeed).
type Seed struct {
	// TenantID is the directory ID; it is also accepted in token URLs.
	TenantID     string       `json:"tenantId"`
	Organization Organization `json:"organization"`
	// Apps are the registered applications that may request tokens.
	Apps  []App  `json:"apps"`
	Users []User `json:"users"`
	Teams []Team `json:"teams"`
	// Chats are chats people already have, with the messages they wrote.
	Chats []Chat `json:"chats,omitempty"`
}

// Organization is the tenant's directory record.
type Organization struct {
	DisplayName string   `json:"displayName"`
	Domains     []Domain `json:"domains"`
}

// Domain is a verified domain. Domain names are accepted in place of the
// tenant ID in token URLs, as Entra ID does.
type Domain struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault,omitempty"`
	IsInitial bool   `json:"isInitial,omitempty"`
}

// App is an application registration.
type App struct {
	ClientID    string `json:"clientId"`
	DisplayName string `json:"displayName,omitempty"`
	// Secrets are accepted client secrets.
	Secrets []string `json:"secrets,omitempty"`
	// ExpiredSecrets are recognized but rejected with AADSTS7000222.
	ExpiredSecrets []string `json:"expiredSecrets,omitempty"`
	// Certificates are the registered certificate credentials.
	Certificates []Certificate `json:"certificates,omitempty"`
	// Roles are the granted application permissions, e.g. "User.Read.All".
	// They become the `roles` claim of issued tokens.
	Roles []string `json:"roles"`
}

// Certificate registers a certificate credential on an App. Provide PEM so
// assertion signatures can be verified. With only a Thumbprint (SHA-1, hex)
// the fake can check the assertion's x5t but not its signature.
type Certificate struct {
	Thumbprint string `json:"thumbprint,omitempty"`
	PEM        string `json:"pem,omitempty"`
}

// User is a directory user.
type User struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail,omitempty"`
	UserPrincipalName string `json:"userPrincipalName"`
	AccountEnabled    bool   `json:"accountEnabled"`
	// UserType is "Member" or "Guest"; empty means "Member".
	UserType string `json:"userType,omitempty"`
	// ProxyAddresses defaults to ["SMTP:<mail>"] when Mail is set.
	ProxyAddresses []string `json:"proxyAddresses,omitempty"`
}

// Team is an existing team.
type Team struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	// Visibility is "public" (default) or "private".
	Visibility string    `json:"visibility,omitempty"`
	Channels   []Channel `json:"channels,omitempty"`
}

// Channel is an existing channel.
type Channel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	// MembershipType is "standard" (default), "private" or "shared".
	MembershipType string `json:"membershipType,omitempty"`
	// CreatedDateTime is when the channel was created; nil means
	// SeedChannelCreated. Imported messages must be later than it, so
	// importing older history needs startMigration with a backdated time.
	CreatedDateTime *time.Time `json:"createdDateTime,omitempty"`
}

// SeedChannelCreated is the creation time of seeded channels that do not
// set one: 2020-01-01T00:00:00Z.
var SeedChannelCreated = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// LoadSeed reads and validates a JSON seed file. Unknown fields are
// rejected so typos do not silently produce an empty tenant.
func LoadSeed(path string) (Seed, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return Seed{}, fmt.Errorf("fakegraph: open seed: %w", err)
	}
	defer f.Close()
	var s Seed
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Seed{}, fmt.Errorf("fakegraph: parse seed %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Seed{}, fmt.Errorf("fakegraph: seed %s: %w", path, err)
	}
	return s, nil
}

// Validate reports every problem that would stop the seed from being served.
func (s Seed) Validate() error {
	_, err := newState(s)
	return err
}

// DefaultRoles are the application permissions the default seed's app
// holds: everything Taut's permission catalog lists.
func DefaultRoles() []string {
	return []string{
		"Teamwork.Migrate.All", "User.Read.All", "Team.Create", "Channel.Create",
		"TeamMember.ReadWrite.All", "ChannelMessage.Read.All", "Files.ReadWrite.All", "Sites.ReadWrite.All",
		"ChannelMember.ReadWrite.All", "Chat.Create", "Chat.ReadBasic.All", "Chat.Read.All",
		"Organization.Read.All", "Team.ReadBasic.All", "Channel.ReadBasic.All",
	}
}

// Identifiers used by DefaultSeed.
const (
	DefaultTenantID = "7f3c1e9a-52d4-4b1e-9a57-0c8f2d6e4b10"
	DefaultClientID = "2b9d4c1f-8e3a-4f6b-b7c2-5a1e0d9f3c84"
	// DefaultSecret is the default app's client secret. It guards nothing.
	DefaultSecret = "fakegraph-local-dev-secret"
)

// DefaultSeed returns a small, fully populated tenant for local development:
// one app holding DefaultRoles with DefaultSecret, a handful of users
// (including a disabled account and a guest) and two teams.
func DefaultSeed() Seed {
	user := func(n int, name, alias string, enabled bool) User {
		return User{
			ID:                fmt.Sprintf("a0000000-0000-4000-8000-%012d", n),
			DisplayName:       name,
			Mail:              alias + "@contoso.example",
			UserPrincipalName: alias + "@contoso.example",
			AccountEnabled:    enabled,
			UserType:          "Member",
		}
	}
	return Seed{
		TenantID: DefaultTenantID,
		Organization: Organization{
			DisplayName: "Contoso Ltd",
			Domains: []Domain{
				{Name: "contoso.example", IsDefault: true},
				{Name: "contoso.onmicrosoft.example", IsInitial: true},
			},
		},
		Apps: []App{{
			ClientID:    DefaultClientID,
			DisplayName: "Taut (local)",
			Secrets:     []string{DefaultSecret},
			Roles:       DefaultRoles(),
		}},
		Users: []User{
			user(1, "Ada Lovelace", "ada", true),
			user(2, "Grace Hopper", "grace", true),
			user(3, "Alan Turing", "alan", true),
			user(4, "Katherine Johnson", "katherine", true),
			user(5, "Edsger Dijkstra", "edsger", false),
			{
				ID: "a0000000-0000-4000-8000-000000000006", DisplayName: "Linus (Fabrikam)",
				Mail: "linus@fabrikam.example", UserPrincipalName: "linus_fabrikam.example#EXT#@contoso.onmicrosoft.example",
				AccountEnabled: true, UserType: "Guest",
			},
			{
				ID: "a0000000-0000-4000-8000-000000000007", DisplayName: "Taut Migration Service",
				UserPrincipalName: "svc-taut@contoso.example", AccountEnabled: true, UserType: "Member",
			},
		},
		Teams: []Team{
			{
				ID: "b0000000-0000-4000-8000-000000000001", DisplayName: "Engineering", Description: "Existing engineering team",
				Channels: []Channel{
					{ID: "19:general-eng@thread.tacv2", DisplayName: "General", MembershipType: "standard"},
					{ID: "19:releases-eng@thread.tacv2", DisplayName: "releases", MembershipType: "standard"},
					{ID: "19:oncall-eng@thread.tacv2", DisplayName: "on-call", MembershipType: "private"},
				},
			},
			{
				ID: "b0000000-0000-4000-8000-000000000002", DisplayName: "Field Operations",
				Channels: []Channel{
					{ID: "19:general-ops@thread.tacv2", DisplayName: "General", MembershipType: "standard"},
				},
			},
		},
	}
}

// state is the server's in-memory tenant. Everything a request can read or
// write lives here, guarded by Server.mu. Messages and per-channel migration
// state hang off channelState.
type state struct {
	tenantID string
	org      Organization
	apps     map[string]*appState // by lower-case client ID
	appOrder []string
	users    []*User
	teams    []*teamState
	chats    []*chatState
	// loseGroupChats is how many group chat creations still lose their
	// response (LoseGroupChatResponses).
	loseGroupChats int
	// chatRefusals and chatStartRefusals make creating a chat with a user,
	// or starting the migration of one, fail with the given message.
	chatRefusals      map[string]string
	chatStartRefusals map[string]string
	// seq numbers the teams, channels, operations, members and opaque
	// message IDs this server creates, so IDs are deterministic.
	seq int
	// items are all drive items by ID and by eTag GUID (upper case);
	// sessions are the open upload sessions by token.
	items       map[string]*driveItemState
	itemsByGUID map[string]*driveItemState
	sessions    map[string]*uploadSession
	// memberAddFail maps a lower-case user ID to the per-user error (code,
	// message) the bulk members/add answers for them. See FailMemberAdd.
	memberAddFail map[string][2]string
}

type appState struct {
	App
	// certs maps the base64url SHA-1 thumbprint (x5t) to the public key;
	// the key is nil for thumbprint-only registrations.
	certs map[string]*rsa.PublicKey
}

type teamState struct {
	Team
	channels []*channelState
	members  []*memberState
	// op is the createTeam operation; nil for seeded teams.
	op *teamOp
	// createdBy is the app that created the team; empty for seeded teams.
	createdBy string
	// migrationMode is "inProgress" for a team created in migration mode
	// until its completeMigration, then "completed"; "" for any other
	// team. migrationOwner is the app that created it that way.
	migrationMode  string
	migrationOwner string
}

// teamOp is the long-running operation behind POST /teams.
type teamOp struct {
	id        string
	createdAt time.Time
	// polls counts GETs of the operation so far.
	polls int
	// fail makes provisioning end in "failed"; the team never appears.
	fail bool
}

type memberState struct {
	id     string
	userID string
	roles  []string
}

type channelState struct {
	Channel
	createdAt time.Time
	// originalCreatedAt is the creation time before the first startMigration.
	originalCreatedAt *time.Time
	// migrationMode is "", "inProgress" or "completed".
	migrationMode string
	// migrationOwner is the app that started the current migration session.
	migrationOwner string
	// messages holds every message of the channel, replies included, by ID.
	messages map[string]*messageState
	// byMillis indexes the same messages by createdDateTime in Unix
	// milliseconds, which Graph requires to be unique within a channel.
	byMillis map[int64]*messageState
	// folder is the channel's files folder, created on first use.
	folder *driveItemState
	// folderCalls counts filesFolder requests, for WithFilesFolderLag.
	folderCalls int
	// members are the channel's own members: private and shared channels
	// only. A standard channel's members are the team's.
	members []*memberState
}

type messageState struct {
	id          string
	replyToID   string
	createdAt   time.Time
	fromUserID  string
	fromName    string
	contentType string
	content     string
	mentions    []MentionView
	importedBy  string
	attachments []AttachmentView
	// hosted* summarize the message's hosted contents; bytes are not kept.
	hostedCount int
	hostedBytes int64
	hostedHash  string
	reactions   []ReactionView
}

func newChannelState(ch Channel, created time.Time) *channelState {
	return &channelState{
		Channel: ch, createdAt: created.UTC(),
		messages: map[string]*messageState{}, byMillis: map[int64]*messageState{},
	}
}

func newState(s Seed) (*state, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	st := &state{tenantID: s.TenantID, org: s.Organization, apps: map[string]*appState{}}
	if strings.TrimSpace(s.TenantID) == "" {
		bad("tenantId is required")
	}
	for i, a := range s.Apps {
		key := strings.ToLower(a.ClientID)
		if key == "" {
			bad("apps[%d]: clientId is required", i)
			continue
		}
		if _, dup := st.apps[key]; dup {
			bad("apps[%d]: duplicate clientId %s", i, a.ClientID)
			continue
		}
		as := &appState{App: a, certs: map[string]*rsa.PublicKey{}}
		for j, c := range a.Certificates {
			x5t, pub, err := c.resolve()
			if err != nil {
				bad("apps[%d].certificates[%d]: %w", i, j, err)
				continue
			}
			as.certs[x5t] = pub
		}
		st.apps[key] = as
		st.appOrder = append(st.appOrder, key)
	}
	seenUser := map[string]bool{}
	for i := range s.Users {
		u := s.Users[i] // copy
		if u.ID == "" || u.UserPrincipalName == "" {
			bad("users[%d]: id and userPrincipalName are required", i)
			continue
		}
		for _, k := range []string{strings.ToLower(u.ID), strings.ToLower(u.UserPrincipalName)} {
			if seenUser[k] {
				bad("users[%d]: duplicate id or userPrincipalName %q", i, k)
			}
			seenUser[k] = true
		}
		if u.UserType == "" {
			u.UserType = "Member"
		}
		if u.ProxyAddresses == nil && u.Mail != "" {
			u.ProxyAddresses = []string{"SMTP:" + u.Mail}
		}
		st.users = append(st.users, &u)
	}
	seenTeam := map[string]bool{}
	for i, t := range s.Teams {
		if t.ID == "" {
			bad("teams[%d]: id is required", i)
			continue
		}
		if seenTeam[t.ID] {
			bad("teams[%d]: duplicate id %s", i, t.ID)
		}
		seenTeam[t.ID] = true
		switch t.Visibility {
		case "":
			t.Visibility = "public"
		case "public", "private":
		default:
			bad("teams[%d]: visibility must be public or private", i)
		}
		ts := &teamState{Team: t}
		ts.Channels = nil
		for j, ch := range t.Channels {
			if ch.ID == "" {
				bad("teams[%d].channels[%d]: id is required", i, j)
				continue
			}
			if ch.MembershipType == "" {
				ch.MembershipType = "standard"
			}
			created := SeedChannelCreated
			if ch.CreatedDateTime != nil {
				created = *ch.CreatedDateTime
			}
			ts.channels = append(ts.channels, newChannelState(ch, created))
		}
		st.teams = append(st.teams, ts)
	}
	for i, c := range s.Chats {
		nextID := func(format string) string {
			st.seq++
			return fmt.Sprintf(format, st.seq)
		}
		if _, err := st.addSeedChat(c, nextID); err != nil {
			bad("chats[%d]: %w", i, err)
		}
	}
	return st, errors.Join(errs...)
}

// resolve returns the certificate's x5t and, when PEM was given, its key.
func (c Certificate) resolve() (x5t string, pub *rsa.PublicKey, err error) {
	if c.PEM != "" {
		var cert *x509.Certificate
		for rest := []byte(c.PEM); ; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type == "CERTIFICATE" {
				if cert, err = x509.ParseCertificate(block.Bytes); err != nil {
					return "", nil, fmt.Errorf("parse certificate: %w", err)
				}
				break
			}
		}
		if cert == nil {
			return "", nil, errors.New("pem contains no CERTIFICATE block")
		}
		rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return "", nil, errors.New("certificate key is not RSA")
		}
		sum := sha1.Sum(cert.Raw) //nolint:gosec // thumbprint
		if c.Thumbprint != "" && !strings.EqualFold(normalizeThumbprint(c.Thumbprint), hex.EncodeToString(sum[:])) {
			return "", nil, errors.New("thumbprint does not match pem")
		}
		return b64(sum[:]), rsaPub, nil
	}
	raw, err := hex.DecodeString(normalizeThumbprint(c.Thumbprint))
	if err != nil || len(raw) != sha1.Size {
		return "", nil, errors.New("thumbprint must be a 40-character hex SHA-1 (or provide pem)")
	}
	return b64(raw), nil, nil
}

func normalizeThumbprint(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(s))
}
