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
	idmap := identity.Resolve(slackUsers, dir, overrides)

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
	for i, t := range m.Target.Teams {
		owner, ok := dir.Find(t.Owner)
		if !ok {
			return permanent(fail.New("plan.owner_missing",
				fmt.Sprintf("The owner of team %q does not exist in Entra", t.Name),
				t.Owner+" was not found in the directory.",
				"Set target.teams[].owner to an existing user, then run `taut migrate --new`."))
		}
		p.Teams = append(p.Teams, store.TeamMapping{
			Index: i, Name: t.Name, Description: t.Description, Visibility: t.Visibility,
			OwnerUPN: owner.UPN, OwnerID: owner.ID,
		})
		lane := teamLane(i)
		p.Lanes = append(p.Lanes, store.Lane{ID: lane, Stage: store.StageTeam, Key: strconv.Itoa(i)})
		p.Batches = append(p.Batches, store.Batch{LaneID: lane, Seq: 0, Key: "create"})
	}

	since, until, err := dayBounds(m)
	if err != nil {
		return permanent(err)
	}
	// The layout was decided when the run was created, from its config
	// (pipeline.LayoutFor); the plan is written under it.
	totals := pipeline.Totals{Layout: runLayout(rs.run), Teams: len(p.Teams), Users: len(idmap.Entries()), Mapped: mapped}
	if totals.Layout != pipeline.LayoutFor(m.Target.DirectMessages.Enabled) {
		return permanent(fmt.Errorf("run %s was created for ledger layout %d, but its config needs layout %d",
			rs.run.ID, totals.Layout, pipeline.LayoutFor(m.Target.DirectMessages.Enabled)))
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
	if m.Target.DirectMessages.Enabled {
		from, to, err := timeBounds(m)
		if err != nil {
			return permanent(err)
		}
		for _, dm := range preflight.DirectMessages(channels, slackUsers, idmap, true) {
			if !dm.Migrates() {
				continue
			}
			ch := dm.Channel
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
			if len(kept) == 0 {
				continue
			}
			// A chat people already use must not be put into migration
			// mode for nothing, so make sure there is something to import
			// before deciding to touch it.
			if ok, err := preflight.HasImportable(ex, ch, from, to); err != nil {
				return permanent(err)
			} else if !ok {
				continue
			}
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
	return slack.Channel{}, fmt.Errorf("channel %s is in the plan but not in the export", id)
}

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
