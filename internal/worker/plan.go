package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
)

// plan is the first stage of a run. It reads the export's index and the
// Entra directory, decides the identity map and the channel layout, and
// writes the whole ledger for the rest of the run in one transaction.
//
// Everything decided here is fixed for the life of the run. Later stages,
// and every resume, read these decisions back rather than recomputing them,
// so a directory change mid-run cannot make two halves of a channel
// disagree about who wrote what.
func (d *Worker) plan(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	m := rs.m

	ex, err := d.openExport(ctx, rs)
	if err != nil {
		return err
	}
	slackUsers, err := ex.Users()
	if err != nil {
		return permanent(err)
	}
	channels, err := ex.Channels()
	if err != nil {
		return permanent(err)
	}

	// --- identity -----------------------------------------------------------
	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	var dirUsers []identity.DirUser
	for u, err := range g.Users(ctx, graph.UserQuery{PageSize: 999}) {
		if err != nil {
			return err
		}
		dirUsers = append(dirUsers, identity.DirUser{
			ID: u.ID, DisplayName: u.DisplayName, Mail: u.Mail, UPN: u.UserPrincipalName,
			Enabled: !u.Disabled(), Guest: strings.EqualFold(u.UserType, "Guest"), Aliases: u.ProxyAddresses,
		})
	}
	dir := identity.NewDirectory(dirUsers)

	overrides, err := d.overrides(ctx, rs)
	if err != nil {
		return err
	}
	idmap := identity.Resolve(slackUsers, dir, overrides, preflight.MatchOptions(m))

	p := store.Plan{}
	mapped := 0
	for _, e := range idmap.Entries() {
		p.Users = append(p.Users, store.UserMapping{
			SlackID: e.SlackID, Name: e.Name, Email: e.Email, Method: string(e.Method),
			EntraID: e.EntraID, EntraUPN: e.EntraUPN, Reason: string(e.Reason),
		})
		if e.Mapped() {
			mapped++
		}
	}
	// Bot messages may name their author by bot ID rather than user ID.
	// Record each bot ID as an alias of its user so both resolve alike.
	seen := map[string]bool{}
	for _, u := range p.Users {
		seen[u.SlackID] = true
	}
	for _, u := range slackUsers {
		if id := u.Profile.BotID; id != "" && !seen[id] {
			seen[id] = true
			p.Users = append(p.Users, store.UserMapping{
				SlackID: id, Name: u.DisplayLabel(), Method: string(identity.MethodNone), Reason: string(identity.ReasonBot),
			})
		}
	}

	// The account that stands in for authors with no Microsoft 365 identity.
	if m.Identity.Unmapped.Policy != config.PolicyBlock {
		acct, ok := dir.Find(m.Identity.Unmapped.Account)
		if !ok {
			return permanent(fail.New("identity.account_missing",
				"The account for unmapped authors does not exist in Entra",
				m.Identity.Unmapped.Account+" was not found in the directory.",
				"Create that user, or point identity.unmapped.account at an existing one, then run `taut migrate --new`."))
		}
		p.Users = append(p.Users, store.UserMapping{
			SlackID: pipeline.PlaceholderUser, Name: acct.DisplayName, Method: "placeholder",
			EntraID: acct.ID, EntraUPN: acct.UPN,
		})
	}

	// --- layout ---------------------------------------------------------------
	lay := preflight.Layout(m, channels, nil)
	if len(lay.Blockers) > 0 {
		return permanent(fail.New("plan.blocked", "The channel plan has a problem `taut preflight` should have caught",
			lay.Blockers[0].Message, lay.Blockers[0].Next))
	}
	since, until, err := dayBounds(m)
	if err != nil {
		return permanent(err)
	}
	// The direct messages that migrate, decided before anything else is
	// laid out: an incremental pass needs to know which of the earlier
	// passes' chats this one continues.
	chats, err := plannedChats(ex, m, channels, slackUsers, idmap, since, until)
	if err != nil {
		return err
	}

	// --- earlier passes -----------------------------------------------------
	// An incremental pass starts from what the pass before it put into
	// Teams. `taut preflight --incremental` reports the same comparison; it
	// is made again here because this is where it is enforced: the export
	// in staging is the one that will be imported, whatever the CLI saw.
	var base *basePass
	if rs.run.Incremental() {
		ids := make([]string, 0, len(chats))
		for _, c := range chats {
			ids = append(ids, c.dm.Channel.ID)
		}
		if base, err = d.loadBase(ctx, rs, lay, ids, channels, slackUsers); err != nil {
			return err
		}
		p.CarryFrom = rs.run.BaseRunID
	}
	p.WorkspaceID = slack.WorkspaceID(slackUsers)

	for i, t := range m.Target.Teams {
		owner, ok := dir.Find(t.Owner)
		if !ok {
			return permanent(fail.New("plan.owner_missing",
				fmt.Sprintf("The owner of team %q does not exist in Entra", t.Name),
				t.Owner+" was not found in the directory.",
				"Set target.teams[].owner to an existing user, then run `taut migrate --new`."))
		}
		tm := store.TeamMapping{
			Index: i, Name: t.Name, Description: t.Description, Visibility: t.Visibility,
			OwnerUPN: owner.UPN, OwnerID: owner.ID,
		}
		if bt, ok := base.team(t.Name); ok {
			// The team an earlier pass created or adopted. Its team lane
			// only confirms that it is still there.
			tm.TeamsID, tm.Adopted = bt.TeamsID, bt.Adopted
		}
		p.Teams = append(p.Teams, tm)
		lane := teamLane(i)
		p.Lanes = append(p.Lanes, store.Lane{ID: lane, Stage: store.StageTeam, Key: strconv.Itoa(i)})
		p.Batches = append(p.Batches, store.Batch{LaneID: lane, Seq: 0, Key: "create"})
	}

	// The layout was decided when the run was created, from its config and,
	// for an incremental pass, from its base (controller.createPass); the
	// plan is written under it.
	totals := pipeline.Totals{Layout: runLayout(rs.run), Teams: len(p.Teams), Users: len(idmap.Entries()), Mapped: mapped}
	if rs.run.Incremental() {
		totals.Pass = rs.run.Pass
	}
	need := pipeline.LayoutFor(m.Target.DirectMessages.Enabled)
	if rs.run.Incremental() {
		need = pipeline.LayoutForPass(m.Target.DirectMessages.Enabled || base.hasChats())
	}
	if totals.Layout != need {
		return permanent(fmt.Errorf("run %s was created for ledger layout %d, but its plan needs layout %d",
			rs.run.ID, totals.Layout, need))
	}
	for _, a := range lay.Assignments {
		ch := a.Channel
		cp := lay.Teams[a.Team].Channels[a.Slot]
		cm := store.ChannelMapping{
			SlackID: ch.ID, SlackName: ch.Name, Kind: string(ch.Kind), TeamIndex: a.Team,
			TargetName: cp.TargetName, IsGeneral: ch.IsGeneral, State: store.ChannelPlanned,
		}
		if ch.Created > 0 {
			cm.Created = time.Unix(ch.Created, 0).UTC()
		}
		if bc, ok := base.channel(ch.ID); ok {
			// The channel an earlier pass created. It keeps its place, its
			// name and its state; the load lane's first batches find
			// nothing left to do, and a finalized channel is put back
			// into migration mode only if there is something to import.
			carry(&cm, bc)
			totals.Continued++
		}
		p.Channels = append(p.Channels, cm)
		totals.Channels++

		days, err := ex.Days(ch)
		if err != nil {
			return permanent(err)
		}
		var kept []string
		for _, day := range days {
			if (since == "" || day >= since) && (until == "" || day <= until) {
				kept = append(kept, day)
			}
		}
		totals.Days += len(kept)

		// The load lane always exists, so that an empty channel is still
		// created. It begins with two provisioning batches. The channel is
		// created first so its files folder exists; migration mode starts
		// only after the channel's files are uploaded, because whether the
		// folder is usable during migration mode is undocumented.
		//
		// The legacy strategy has the same lanes and batches. There the
		// channel is created already in migration mode, so its files are
		// uploaded during it and the second batch has nothing left to do.
		load := lane(store.StageLoad, ch.ID)
		p.Lanes = append(p.Lanes, store.Lane{ID: load, Stage: store.StageLoad, Key: ch.ID})
		create := store.Batch{LaneID: load, Seq: loadSeqCreate, Key: "create",
			DependsOn: []store.BatchRef{{LaneID: teamLane(a.Team), Seq: 0}}}
		open := store.Batch{LaneID: load, Seq: loadSeqOpen, Key: "open"}
		if len(kept) == 0 {
			p.Batches = append(p.Batches, create, open)
			continue
		}
		extract, transform, files := lane(store.StageExtract, ch.ID), lane(store.StageTransform, ch.ID), lane(store.StageFiles, ch.ID)
		// Migration mode waits for the last files batch; files batches run
		// in order, so that is all of them.
		open.DependsOn = []store.BatchRef{{LaneID: files, Seq: len(kept) - 1}}
		p.Batches = append(p.Batches, create, open)
		p.Lanes = append(p.Lanes,
			store.Lane{ID: extract, Stage: store.StageExtract, Key: ch.ID},
			store.Lane{ID: transform, Stage: store.StageTransform, Key: ch.ID},
			store.Lane{ID: files, Stage: store.StageFiles, Key: ch.ID})
		for i, day := range kept {
			p.Batches = append(p.Batches,
				store.Batch{LaneID: extract, Seq: i, Key: day},
				store.Batch{LaneID: transform, Seq: i, Key: day,
					DependsOn: []store.BatchRef{{LaneID: extract, Seq: i}}},
				store.Batch{LaneID: files, Seq: i, Key: day,
					DependsOn: []store.BatchRef{{LaneID: extract, Seq: i}, {LaneID: load, Seq: loadSeqCreate}}},
				store.Batch{LaneID: load, Seq: loadSeqDays + i, Key: day,
					DependsOn: []store.BatchRef{{LaneID: transform, Seq: i}}},
			)
		}
	}
	// --- direct messages ------------------------------------------------------
	// Each DM and group DM that migrates is a conversation with no team,
	// imported into a chat. It has the lanes of a channel but
	// for files, which are not moved for a chat: extract and transform per
	// day, and a load lane that begins by creating (or finding) the chat
	// and putting it into migration mode.
	for _, c := range chats {
		dm, ch, kept := c.dm, c.dm.Channel, c.days
		// Slack's name for a group DM lists the handle of everyone in
		// it. It is not kept: the label is all the ledger holds about
		// who is in a conversation, and that goes at purge.
		cm := store.ChannelMapping{
			SlackID: ch.ID, Kind: string(ch.Kind), TeamIndex: store.NoTeam,
			TargetName: dm.Label, Members: dm.Members, Topic: dm.Topic, State: store.ChannelPlanned,
		}
		if ch.Created > 0 {
			cm.Created = time.Unix(ch.Created, 0).UTC()
		}
		if bc, ok := base.channel(ch.ID); ok {
			label := cm.TargetName
			carry(&cm, bc)
			// The label is this pass's own: a purge of the earlier pass
			// erases the one it kept.
			cm.TargetName = label
			totals.Continued++
		}
		p.Channels = append(p.Channels, cm)
		totals.Chats++
		totals.Days += len(kept)

		extract, transform, load := lane(store.StageExtract, ch.ID), lane(store.StageTransform, ch.ID), lane(store.StageLoad, ch.ID)
		p.Lanes = append(p.Lanes,
			store.Lane{ID: load, Stage: store.StageLoad, Key: ch.ID},
			store.Lane{ID: extract, Stage: store.StageExtract, Key: ch.ID},
			store.Lane{ID: transform, Stage: store.StageTransform, Key: ch.ID})
		p.Batches = append(p.Batches,
			store.Batch{LaneID: load, Seq: loadSeqCreate, Key: "create"},
			store.Batch{LaneID: load, Seq: loadSeqOpen, Key: "open"})
		for i, day := range kept {
			p.Batches = append(p.Batches,
				store.Batch{LaneID: extract, Seq: i, Key: day},
				store.Batch{LaneID: transform, Seq: i, Key: day,
					DependsOn: []store.BatchRef{{LaneID: extract, Seq: i}}},
				store.Batch{LaneID: load, Seq: loadSeqDays + i, Key: day,
					DependsOn: []store.BatchRef{{LaneID: transform, Seq: i}}},
			)
		}
	}
	// --- conversations only the earlier passes know -------------------------
	// A conversation an earlier pass put into Teams that this pass's plan
	// does not contain (it is gone from Slack, or a direct message no
	// longer qualifies) still belongs to the migration: it may be in
	// migration mode, and only a finalize of the latest pass will complete
	// it. It is kept in the ledger with no lanes, so nothing is imported
	// into it and `taut finalize` still finds it.
	if base != nil {
		for _, lc := range base.cont.Dormant {
			bc, ok := base.channels[lc.SlackID]
			if !ok {
				continue
			}
			cm := store.ChannelMapping{
				SlackID: bc.SlackID, SlackName: bc.SlackName, Kind: bc.Kind, TeamIndex: store.NoTeam,
				TargetName: bc.TargetName, IsGeneral: bc.IsGeneral, Created: bc.Created,
				Members: bc.Members, Topic: bc.Topic,
			}
			if !bc.IsChat() {
				ti, ok := teamIndex(m, base.teamName[bc.TeamIndex])
				if !ok {
					// Continue reports this as a blocker; it is checked
					// before the plan gets here.
					return permanent(fmt.Errorf("channel %s belongs to a team that is no longer in the config file", bc.SlackID))
				}
				cm.TeamIndex = ti
			}
			carry(&cm, bc)
			p.Channels = append(p.Channels, cm)
			totals.Dormant++
		}
	}
	if totals.Channels == 0 && totals.Chats == 0 {
		return permanent(fail.New("plan.empty", "No channels are selected for migration", "",
			"Check target.teams[].channels in the config file against the export, then run `taut migrate --new`."))
	}
	p.Totals, _ = json.Marshal(totals)

	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.ApplyPlan(ctx, p); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, b.Seq, "", totals.Channels+totals.Chats, p.Totals)
	})
}

// plannedChat is a direct-message conversation the run migrates, with the
// days of it that are in range.
type plannedChat struct {
	dm   preflight.DMPlan
	days []string
}

// plannedChats decides which direct messages and group DMs the run migrates:
// those whose participants all qualify (preflight.DirectMessages) and that
// have something to import in the config file's date range.
func plannedChats(ex *export.Export, m *config.Config, channels []slack.Channel, users []slack.User, idmap *identity.Map, since, until string) ([]plannedChat, error) {
	if !m.Target.DirectMessages.Enabled {
		return nil, nil
	}
	from, to, err := timeBounds(m)
	if err != nil {
		return nil, permanent(err)
	}
	var out []plannedChat
	for _, dm := range preflight.DirectMessages(channels, users, idmap, true) {
		if !dm.Migrates() {
			continue
		}
		days, err := ex.Days(dm.Channel)
		if err != nil {
			return nil, permanent(err)
		}
		var kept []string
		for _, day := range days {
			if (since == "" || day >= since) && (until == "" || day <= until) {
				kept = append(kept, day)
			}
		}
		if len(kept) == 0 {
			continue
		}
		// A chat people already use must not be put into migration
		// mode for nothing, so make sure there is something to import
		// before deciding to touch it.
		if ok, err := preflight.HasImportable(ex, dm.Channel, from, to); err != nil {
			return nil, permanent(err)
		} else if !ok {
			continue
		}
		out = append(out, plannedChat{dm: dm, days: kept})
	}
	return out, nil
}

// basePass is what an incremental pass takes over from the pass before it.
type basePass struct {
	run store.Run
	// cont is how this pass's plan lines up with the base's ledger.
	cont *preflight.Continuation
	// teams are the base's teams by lower-cased name; teamName its team
	// names by index.
	teams    map[string]store.TeamMapping
	teamName map[int]string
	// channels are the base's conversations that are in Teams, by Slack ID.
	channels map[string]store.ChannelMapping
}

// team returns the base's team of this name, if it exists in Teams. It is
// safe on a nil basePass, which is what a first pass has.
func (b *basePass) team(name string) (store.TeamMapping, bool) {
	if b == nil {
		return store.TeamMapping{}, false
	}
	t, ok := b.teams[strings.ToLower(name)]
	return t, ok && t.TeamsID != ""
}

// channel returns the base's row for a conversation this pass continues.
func (b *basePass) channel(slackID string) (store.ChannelMapping, bool) {
	if b == nil {
		return store.ChannelMapping{}, false
	}
	if _, ok := b.cont.Continued[slackID]; !ok {
		return store.ChannelMapping{}, false
	}
	c, ok := b.channels[slackID]
	return c, ok
}

// hasChats reports whether the base's ledger holds a chat, which this
// pass's ledger then holds too, whatever its own config says.
func (b *basePass) hasChats() bool {
	if b == nil {
		return false
	}
	// Read from the base's layout, as the controller reads it when it
	// creates the pass, so the two cannot disagree.
	return pipeline.LayoutHasChats(runLayout(b.run))
}

// carry makes cm the continuation of the base's conversation bc: the same
// channel or chat in Teams, in the state the base left it.
func carry(cm *store.ChannelMapping, bc store.ChannelMapping) {
	cm.Carried = true
	cm.TeamsID, cm.State, cm.MigrationStart = bc.TeamsID, bc.State, bc.MigrationStart
	cm.FilesDriveID, cm.FilesFolderID = bc.FilesDriveID, bc.FilesFolderID
	if !bc.IsChat() {
		// The channel keeps the name it has in Teams.
		cm.TargetName = bc.TargetName
	}
}

func teamIndex(m *config.Config, name string) (int, bool) {
	for i, t := range m.Target.Teams {
		if strings.EqualFold(t.Name, name) {
			return i, true
		}
	}
	return 0, false
}

// loadBase reads the pass an incremental pass follows and checks that this
// pass may follow it: the same workspace, the same tenant, every migrated
// conversation still where it was. A failure here is permanent and nothing
// has been written: the operator corrects the config file or the export and
// starts the pass again.
func (d *Worker) loadBase(ctx context.Context, rs *runState, lay *preflight.PlanLayout, chats []string, channels []slack.Channel, users []slack.User) (*basePass, error) {
	st := d.cfg.Store
	run, err := st.GetRun(ctx, rs.run.BaseRunID)
	if err != nil {
		return nil, err
	}
	ledger, err := st.Ledger(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	tenant := ""
	if bm, err := config.Parse(run.Config); err == nil {
		tenant = bm.Target.Entra.TenantID
	}
	cont := preflight.Continue(rs.m, lay, chats, channels, users, preflight.Base{Ledger: ledger, TenantID: tenant})
	if len(cont.Blockers) > 0 {
		f := cont.Blockers[0]
		return nil, permanent(fail.New("plan."+strings.TrimPrefix(f.Code, "incremental."),
			"This incremental pass cannot follow run "+run.ID, f.Message,
			f.Next+" Then run `taut migrate --incremental --new`: this pass has written nothing."))
	}
	b := &basePass{run: run, cont: cont, teams: map[string]store.TeamMapping{}, teamName: map[int]string{}, channels: map[string]store.ChannelMapping{}}
	teams, err := st.Teams(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		b.teams[strings.ToLower(t.Name)] = t
		b.teamName[t.Index] = t.Name
	}
	chans, err := st.Channels(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range chans {
		if c.TeamsID == "" {
			continue
		}
		switch c.State {
		case store.ChannelCreated, store.ChannelMigrating, store.ChannelCompleted:
			b.channels[c.SlackID] = c
		}
	}
	return b, nil
}

// The first batches of a load lane, for a channel and for a chat alike.
const (
	loadSeqCreate = 0 // the channel (or chat) exists in Teams
	loadSeqOpen   = 1 // it is in migration mode
	loadSeqDays   = 2 // first day batch; day i is batch loadSeqDays+i
)

func lane(stage, channelID string) string { return stage + ":" + channelID }

// outputKey is where a batch attempt stages its output. The lease's fence
// is part of the key, so a worker that lost its lease mid-batch can never
// overwrite the output a later holder committed: each attempt writes its own
// object, and only the one whose transaction commits is recorded on the
// batch and read downstream.
func outputKey(runID, stage, channelID string, seq int, fence int64) string {
	return staging.BatchKey(runID, stage, channelID, seq) + ".f" + strconv.FormatInt(fence, 10)
}

// inputKey returns the recorded output of the upstream batch that feeds
// batch seq of a channel.
func (d *Worker) inputKey(ctx context.Context, runID, upstreamStage, channelID string, seq int) (string, error) {
	up, err := d.cfg.Store.GetBatch(ctx, runID, store.BatchRef{LaneID: lane(upstreamStage, channelID), Seq: seq})
	if err != nil {
		return "", err
	}
	if up.Status != store.BatchDone || up.Output == "" {
		return "", fmt.Errorf("upstream %s batch %d of %s has no recorded output yet", upstreamStage, seq, channelID)
	}
	return up.Output, nil
}
func teamLane(index int) string { return store.StageTeam + ":" + strconv.Itoa(index) }

// dayBounds returns the config file's date range as day keys, widened by a day
// on each side. Day files are named by local date in a timezone the export
// does not state, so the day filter must be loose; the extract stage then
// filters each message by its exact timestamp.
func dayBounds(m *config.Config) (since, until string, err error) {
	if s := m.Content.Since; s != "" {
		t, err := config.ParseDate(s)
		if err != nil {
			return "", "", err
		}
		since = t.AddDate(0, 0, -1).Format(config.DateLayout)
	}
	if s := m.Content.Until; s != "" {
		t, err := config.ParseDate(s)
		if err != nil {
			return "", "", err
		}
		until = t.AddDate(0, 0, 1).Format(config.DateLayout)
	}
	return since, until, nil
}

// timeBounds returns the config file's date range as exact instants: since is
// inclusive, until exclusive (the end of the last day).
func timeBounds(m *config.Config) (since, until time.Time, err error) {
	if s := m.Content.Since; s != "" {
		if since, err = config.ParseDate(s); err != nil {
			return
		}
	}
	if s := m.Content.Until; s != "" {
		if until, err = config.ParseDate(s); err != nil {
			return
		}
		until = until.AddDate(0, 0, 1)
	}
	return
}

// overrides loads the identity override CSV that `taut migrate` uploaded with
// the export, if there is one.
func (d *Worker) overrides(ctx context.Context, rs *runState) ([]identity.Override, error) {
	r, err := rs.blobs.Get(ctx, pipeline.OverridesKey(rs.run.ID))
	if errors.Is(err, staging.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	ov, err := identity.ParseOverrides(r)
	if err != nil {
		return nil, permanent(err)
	}
	return ov, nil
}

// extract turns one day file of the export into a normalized batch in
// staging. It decides nothing about content; it only drops messages outside
// the config file's date range and entries the export reader could not parse.
func (d *Worker) extract(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	ex, err := d.openExport(ctx, rs)
	if err != nil {
		return err
	}
	ch, err := exportChannel(ex, lease.Key)
	if err != nil {
		return permanent(err)
	}
	msgs, derr := ex.DayMessages(ch, b.Key)
	malformed := 0
	if derr != nil {
		// DayMessages returns the readable messages together with one
		// error per unreadable entry. Anything else means the day file
		// itself is unusable.
		// A truncated or corrupt file also yields the messages before the
		// break; importing those and calling the day done would lose the
		// rest silently, so that fails the batch instead.
		var bad error
		malformed, bad = entryErrors(derr)
		if bad != nil {
			return permanent(bad)
		}
	}
	since, until, err := timeBounds(rs.m)
	if err != nil {
		return permanent(err)
	}

	key := outputKey(rs.run.ID, store.StageExtract, ch.ID, b.Seq, lease.Fence)
	outOfRange := 0
	n, err := writeBatch(ctx, rs.blobs, key, func(w *pipeline.Writer) error {
		for _, msg := range msgs {
			ts := msg.Time()
			if (!since.IsZero() && ts.Before(since)) || (!until.IsZero() && !ts.Before(until)) {
				outOfRange++
				continue
			}
			if err := w.Write(msg); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	stats, _ := json.Marshal(map[string]int{"malformed": malformed, "outOfRange": outOfRange})
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		return tx.CompleteBatch(ctx, b.Seq, key, n, stats)
	})
}

// exportChannel finds a channel in the export by Slack ID.
func exportChannel(ex *export.Export, id string) (slack.Channel, error) {
	chans, err := ex.Channels()
	if err != nil {
		return slack.Channel{}, err
	}
	for _, c := range chans {
		if c.ID == id {
			return c, nil
		}
	}
	return slack.Channel{}, fmt.Errorf("channel %s is in the plan but %w", id, errNotInExport)
}

// errNotInExport is exportChannel's answer for a conversation the export
// does not list.
var errNotInExport = errors.New("not in the export")

// entryErrors walks a joined error from the export reader. It returns how
// many leaves are per-entry errors (one unreadable message each) and the
// first leaf that is anything else, which means the day file as a whole
// cannot be trusted.
func entryErrors(err error) (entries int, other error) {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			n, o := entryErrors(e)
			entries += n
			if other == nil {
				other = o
			}
		}
		return entries, other
	}
	var me *export.MessageError
	if errors.As(err, &me) {
		return 1, nil
	}
	return 0, err
}
