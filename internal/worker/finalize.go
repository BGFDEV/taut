package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/store"
)

// Audit log operations of the finalizing stages.
const (
	opChannelComplete = "channel.complete_migration"
	opMemberAdd       = "member.add"
	opMemberSkip      = "member.skip"
	// A person's membership of one private channel. The source is the
	// Slack channel and user ("C…/U…"), the target the Teams channel and
	// the Entra user ("19:…/…"), so an entry says which channel on both
	// sides.
	opChannelMemberAdd  = "channel_member.add"
	opChannelMemberSkip = "channel_member.skip"
	// Legacy strategy only: the team's own migration is completed, and
	// the team, which was created without members, gets its owner.
	opTeamComplete = "team.complete_migration"
	opOwnerAdd     = "member.add_owner"
)

func membersLane(index int) string { return store.StageMembers + ":" + strconv.Itoa(index) }

// FinalizeSelection says what a finalize request will do.
type FinalizeSelection struct {
	Plan store.FinalizePlan
	// Channels is how many channels the request selected and Chats how
	// many direct-message conversations; Already how many of both had
	// completed migration before it.
	Channels int
	Chats    int
	Already  int
	// Teams is how many teams will get their members.
	Teams int
}

// PlanFinalize builds the finalizing work for a run: one lane per selected
// channel that completes its migration mode, and one lane per team that
// adds its members.
//
// A team gets its members only once every one of its channels is completed
// or selected. Until then some of its channels are still locked for
// import, and inviting people into a half-open team would only confuse
// them.
//
// A direct message's chat gets a finalize lane like a channel. It belongs
// to no team and its participants were set when it was created, so it
// takes no part in the members lanes: a team's members wait for the
// team's channels only.
//
// selected lists Slack channel IDs; empty means every channel and every
// direct message. It returns an error naming the first selected ID that is
// not a conversation of the run.
//
// inProgress lists the channels that an earlier request already queued for
// completion (those with a finalize lane). They count as selected: an earlier
// partial finalize that has not finished yet must not hide from this one, or
// two overlapping requests could each conclude the team is still half
// locked and neither would add its members.
func PlanFinalize(channels []store.ChannelMapping, teams []store.TeamMapping, selected []string, inProgress map[string]bool, retain bool) (FinalizeSelection, error) {
	byID := make(map[string]store.ChannelMapping, len(channels))
	for _, c := range channels {
		byID[c.SlackID] = c
	}
	want := map[string]bool{}
	if len(selected) == 0 {
		for _, c := range channels {
			want[c.SlackID] = true
		}
	}
	for _, id := range selected {
		if _, ok := byID[id]; !ok {
			return FinalizeSelection{}, fmt.Errorf("channel %q is not part of this run", id)
		}
		want[id] = true
	}

	sel := FinalizeSelection{Plan: store.FinalizePlan{Retain: retain}}
	for id := range want {
		if byID[id].IsChat() {
			sel.Chats++
		} else {
			sel.Channels++
		}
	}
	for id := range inProgress {
		if _, ok := byID[id]; ok {
			want[id] = true
		}
	}
	// Stable order, so that repeating a request builds the same plan.
	ordered := append([]store.ChannelMapping(nil), channels...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SlackID < ordered[j].SlackID })

	// open counts, per team, the channels that will still be in migration
	// mode after this request.
	open := map[int]int{}
	deps := map[int][]store.BatchRef{}
	for _, c := range ordered {
		switch {
		case c.State == store.ChannelCompleted, c.State == store.ChannelSkipped:
			// A skipped conversation has no chat in migration mode:
			// nothing to finalize, and nothing that holds the run open.
			if want[c.SlackID] {
				sel.Already++
			}
		case want[c.SlackID]:
			id := lane(store.StageFinalize, c.SlackID)
			sel.Plan.Lanes = append(sel.Plan.Lanes, store.Lane{ID: id, Stage: store.StageFinalize, Key: c.SlackID})
			sel.Plan.Batches = append(sel.Plan.Batches, store.Batch{LaneID: id, Seq: 0, Key: "complete"})
			if !c.IsChat() {
				deps[c.TeamIndex] = append(deps[c.TeamIndex], store.BatchRef{LaneID: id, Seq: 0})
			}
		case !c.IsChat():
			open[c.TeamIndex]++
		}
	}
	for _, t := range teams {
		if open[t.Index] > 0 {
			continue
		}
		id := membersLane(t.Index)
		sel.Plan.Lanes = append(sel.Plan.Lanes, store.Lane{ID: id, Stage: store.StageMembers, Key: strconv.Itoa(t.Index)})
		sel.Plan.Batches = append(sel.Plan.Batches, store.Batch{LaneID: id, Seq: 0, Key: "add", DependsOn: deps[t.Index]})
		sel.Teams++
	}
	return sel, nil
}

// finalize completes one channel's migration mode, which unlocks it for
// people. It is the one step of a migration that cannot be undone: after
// it, nothing more can be imported into the channel with its original
// timestamps.
//
// It is safe to repeat. Graph refuses a second completeMigration with "not
// in migration mode", which here means the work is already done.
func (d *Worker) finalize(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, lease.Key)
	if err != nil {
		return err
	}
	if cm.State == store.ChannelSkipped {
		return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 0, nil) })
	}
	if cm.TeamsID == "" {
		return permanent(fmt.Errorf("%s was never created in Teams, so there is nothing to complete", conversation(cm)))
	}
	// A direct message's chat is completed the same way, by its own call.
	// It was visible to its participants all along; completing it only
	// ends migration mode.
	detail, op := "", opChannelComplete
	if cm.IsChat() {
		op = opChatComplete
	}
	if cm.State != store.ChannelCompleted {
		teamID := ""
		if !cm.IsChat() {
			if teamID, err = d.teamID(ctx, rs, cm.TeamIndex); err != nil {
				return err
			}
		}
		g, err := d.client(ctx, rs)
		if err != nil {
			return err
		}
		if err := d.tenant.Wait(ctx); err != nil {
			return err
		}
		complete := func() error { return g.CompleteChannelMigration(ctx, teamID, cm.TeamsID) }
		if cm.IsChat() {
			complete = func() error { return g.CompleteChatMigration(ctx, cm.TeamsID) }
		}
		switch err := complete(); {
		case err == nil:
		case graph.IsNotInMigration(err):
			detail = "already out of migration mode"
		default:
			return err
		}
	}
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if cm.State != store.ChannelCompleted {
			if err := tx.SetChannel(ctx, cm.SlackID, cm.TeamsID, store.ChannelCompleted, cm.MigrationStart); err != nil {
				return err
			}
			e := d.entry(lease, op, cm.SlackID, cm.TeamsID, "")
			e.Detail = detail
			if err := tx.Log(ctx, e); err != nil {
				return err
			}
		}
		return tx.CompleteBatch(ctx, b.Seq, "", 1, nil)
	})
	if err != nil {
		return err
	}
	if cm.State != store.ChannelCompleted {
		d.cfg.Observer.Count(ctx, store.StageFinalize, EventChannelFinalized, 1)
	}
	// Other stages cache channel states; this one changed.
	rs.mu.Lock()
	rs.users, rs.channels = nil, nil
	rs.mu.Unlock()
	return nil
}

// membersCursor is what the members lane remembers between attempts: who
// was already there before Taut added anyone. On a later attempt that tells
// apart the people who were always there (not Taut's doing, not logged)
// from those an interrupted attempt added but did not get to record.
type membersCursor struct {
	// Team is the team's membership before Taut's first addition; nil
	// until it has been read.
	Team []string `json:"team"`
	// Channels is the same for each private channel, by Slack channel ID.
	Channels map[string][]string `json:"channels,omitempty"`
}

func readMembersCursor(raw json.RawMessage) (membersCursor, error) {
	var cur membersCursor
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return cur, nil
	case strings.HasPrefix(trimmed, "["):
		// The shape before private channels: the team's baseline alone.
		if err := json.Unmarshal(raw, &cur.Team); err != nil {
			return cur, err
		}
		if cur.Team == nil {
			cur.Team = []string{}
		}
		return cur, nil
	}
	return cur, json.Unmarshal(raw, &cur)
}

func idSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

func sortedIDs(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// channelMemberTarget is the audit log's name for one person's membership
// of one private channel: the Teams channel and the Entra user.
func channelMemberTarget(teamsChannelID, entraID string) string {
	return teamsChannelID + "/" + entraID
}

// membersLog is what earlier attempts of a members lane already recorded.
type membersLog struct {
	// team holds the Entra IDs settled for the team (added or skipped);
	// skipped the ones that could not be added, with the reason.
	team    map[string]bool
	skipped map[string]string
	// channel holds the channel memberships settled, by
	// channelMemberTarget.
	channel map[string]bool
	// owner is true once the team's owner was recorded as added (legacy
	// strategy).
	owner bool
	// ops counts entries per operation.
	ops map[string]int
}

func (d *Worker) readMembersLog(ctx context.Context, lease *store.Lease) (membersLog, error) {
	l := membersLog{team: map[string]bool{}, skipped: map[string]string{}, channel: map[string]bool{}, ops: map[string]int{}}
	for after := int64(0); ; {
		page, err := d.cfg.Store.LogEntries(ctx, lease.RunID, lease.LaneID, after, 1000)
		if err != nil {
			return l, err
		}
		for _, e := range page {
			l.ops[e.Op]++
			switch e.Op {
			case opMemberAdd:
				l.team[e.TargetID] = true
			case opMemberSkip:
				l.team[e.TargetID] = true
				l.skipped[e.TargetID] = e.Detail
			case opChannelMemberAdd, opChannelMemberSkip:
				l.channel[e.TargetID] = true
			case opOwnerAdd:
				l.owner = true
			}
			after = e.Seq
		}
		if len(page) < 1000 {
			return l, nil
		}
	}
}

// logAll appends entries to the lane's audit log, a transaction per 200.
func (d *Worker) logAll(ctx context.Context, lease *store.Lease, entries []store.LogEntry) error {
	for len(entries) > 0 {
		n := min(len(entries), 200)
		part := entries[:n]
		err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
			for _, e := range part {
				if err := tx.Log(ctx, e); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		entries = entries[n:]
	}
	return nil
}

// completeTeam ends the migration of a team that was created in migration
// mode (the legacy strategy), which is what lets it take members. It
// returns how many audit entries it recorded.
//
// Graph completes a team only once every one of its channels has completed
// its own migration. The channels Taut imported into were completed by
// their finalize lanes, which this lane waits for. The team's General
// channel exists whether or not a Slack channel was imported into it, so
// when none was it is completed here.
//
// It is safe to repeat: a second completeMigration is refused as "not in
// migration mode", which here means it is done, and each step is recorded
// once. Completing a team is final.
func (d *Worker) completeTeam(ctx context.Context, lease *store.Lease, g Graph, team store.TeamMapping, chans map[string]store.ChannelMapping, logged membersLog) (int, error) {
	if logged.ops[opTeamComplete] > 0 {
		return 0, nil
	}
	recorded := 0
	generalImported := false
	for _, cm := range chans {
		if cm.TeamIndex == team.Index && (cm.IsGeneral || strings.EqualFold(cm.TargetName, teamsGeneral)) {
			generalImported = true
		}
	}
	if !generalImported && logged.ops[opChannelComplete] == 0 {
		if err := d.tenant.Wait(ctx); err != nil {
			return recorded, err
		}
		general, found, err := g.FindChannel(ctx, team.TeamsID, teamsGeneral)
		if err != nil {
			return recorded, fmt.Errorf("looking for the General channel of team %q: %w", team.Name, err)
		}
		// A General that never was in migration mode needs nothing.
		if found && general.MigrationMode != "" {
			e := d.entry(lease, opChannelComplete, "team/"+lease.Key+"/general", general.ID, "")
			e.Detail = "the team's General channel; no Slack channel was imported into it"
			if general.MigrationMode == graph.MigrationInProgress {
				if err := g.CompleteChannelMigration(ctx, team.TeamsID, general.ID); err != nil && !graph.IsNotInMigration(err) {
					return recorded, fmt.Errorf("completing the General channel of team %q: %w", team.Name, err)
				}
			}
			if err := d.logAll(ctx, lease, []store.LogEntry{e}); err != nil {
				return recorded, err
			}
			recorded++
		}
	}

	if err := d.tenant.Wait(ctx); err != nil {
		return recorded, err
	}
	e := d.entry(lease, opTeamComplete, "team/"+lease.Key, team.TeamsID, "")
	switch err := g.CompleteTeamMigration(ctx, team.TeamsID); {
	case err == nil:
	case graph.IsNotInMigration(err):
		e.Detail = "already out of migration mode"
	default:
		return recorded, fmt.Errorf("completing the migration of team %q: %w", team.Name, err)
	}
	if err := d.logAll(ctx, lease, []store.LogEntry{e}); err != nil {
		return recorded, err
	}
	return recorded + 1, nil
}

// members adds a team's people once its channels are open.
//
// Under the legacy strategy it first completes the team's own migration
// and adds the team's owner: a team created in migration mode has nobody
// in it.
//
// The team's members are the union of the Slack members of its migrated
// channels, standard and private alike, mapped to Microsoft 365 accounts
// by the run's identity mapping. People with no account are not added (the
// preflight listed them). Members of a private channel must join the team
// too: Teams only lets members of the team into its private
// channels. Each private channel then gets its own Slack members, and
// nobody else.
//
// Anyone already in the team or a channel is left exactly as they are.
// What Graph does with someone who is posted a second time is not
// documented, posting an existing owner again as a member can demote them,
// and an adopted team may have owners Taut knows nothing about. So the
// stage reads the current membership first and only adds who is missing.
// That also makes a repeated attempt cheap: it continues where the last
// one stopped.
//
// Team members are added up to 200 per request. Graph allows four member
// requests per minute per team, single or bulk, so one request per person
// would take a real team hours.
//
// Each member is recorded as they are added, and the audit log lists each
// one once however many attempts it took.
func (d *Worker) members(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) (err error) {
	recorded := 0
	defer func() { err = withProgress(err, recorded) }()

	index, err := strconv.Atoi(lease.Key)
	if err != nil {
		return permanent(fmt.Errorf("members lane %q does not name a team", lease.LaneID))
	}
	team, err := d.teamMapping(ctx, rs, index)
	if err != nil {
		return err
	}
	if team.TeamsID == "" {
		return permanent(fmt.Errorf("team %d was never created, so it has no members to add", index))
	}
	users, chans, err := d.mappings(ctx, rs)
	if err != nil {
		return err
	}
	ex, err := d.openExport(ctx, rs)
	if err != nil {
		return err
	}

	// Membership is access. The identity mapping exists to attribute
	// messages, and for that it maps anyone with a matching account; here
	// only full, current, human members of the workspace qualify. Guests
	// were confined to particular channels in Slack, deactivated people
	// had no access at all, and Slack Connect users belong to another
	// organization.
	exUsers, err := ex.Users()
	if err != nil {
		return permanent(err)
	}
	ineligible := map[string]bool{}
	for _, u := range exUsers {
		if !memberEligible(u) {
			ineligible[u.ID] = true
		}
	}

	// Entra object ID → the Slack user it stands for: for the team, and
	// for each private channel by its Slack ID.
	people := map[string]string{}
	inChannel := map[string]map[string]string{}
	var private []string
	unmapped := map[string]bool{}
	excluded := 0
	for id, cm := range chans {
		if cm.TeamIndex != index {
			continue
		}
		ch, err := exportChannel(ex, id)
		if cm.Carried && errors.Is(err, errNotInExport) {
			// A channel an earlier pass migrated that this pass's export no
			// longer has: it was deleted in Slack since. The export that
			// listed its members went with the earlier pass, so nobody
			// becomes a member on its account.
			continue
		}
		if err != nil {
			return permanent(err)
		}
		var own map[string]string
		if cm.Kind == config.ChannelPrivate {
			own = map[string]string{}
			inChannel[id] = own
			private = append(private, id)
		}
		for _, slackID := range ch.Members {
			if ineligible[slackID] {
				excluded++
				continue
			}
			u, ok := users[slackID]
			if !ok || u.EntraID == "" {
				unmapped[slackID] = true
				continue
			}
			if prev, dup := people[u.EntraID]; !dup || slackID < prev {
				people[u.EntraID] = slackID
			}
			if own != nil {
				if prev, dup := own[u.EntraID]; !dup || slackID < prev {
					own[u.EntraID] = slackID
				}
			}
		}
	}
	sort.Strings(private)

	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	cur, err := readMembersCursor(lease.Cursor)
	if err != nil {
		return permanent(fmt.Errorf("members lane %q has an unreadable cursor: %w", lease.LaneID, err))
	}
	saveCursor := func() error {
		raw, _ := json.Marshal(cur)
		if err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.SetCursor(ctx, raw) }); err != nil {
			return err
		}
		lease.Cursor = raw
		return nil
	}

	// What earlier attempts already recorded.
	logged, err := d.readMembersLog(ctx, lease)
	if err != nil {
		return err
	}
	legacy := isLegacy(rs)
	if legacy {
		// A team created in migration mode takes no members until its
		// own migration is completed.
		n, err := d.completeTeam(ctx, lease, g, team, chans, logged)
		recorded += n
		if err != nil {
			return err
		}
	}

	// Who is in the team now. A failure here (the team is gone, say) is a
	// failure of the lane, never a reason to skip people.
	if err := d.tenant.Wait(ctx); err != nil {
		return err
	}
	present := map[string]bool{}
	for m, err := range g.TeamMembers(ctx, team.TeamsID) {
		if err != nil {
			return fmt.Errorf("reading the members of team %q: %w", team.Name, err)
		}
		present[m.UserID] = true
	}
	// The membership as it was before Taut added anyone is kept in the
	// lane's cursor, written before the first addition.
	if cur.Team == nil {
		cur.Team = sortedIDs(present)
		if err := saveCursor(); err != nil {
			return err
		}
	}
	baseline := idSet(cur.Team)

	var ids []string
	for id := range people {
		if !baseline[id] && !logged.team[id] && id != team.OwnerID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	added, considered := 0, len(ids)
	record := func(entries []store.LogEntry) error {
		if err := d.logAll(ctx, lease, entries); err != nil {
			return err
		}
		for _, e := range entries {
			recorded++
			switch e.Op {
			case opMemberAdd:
				added++
				logged.team[e.TargetID] = true
			case opMemberSkip:
				logged.team[e.TargetID] = true
				logged.skipped[e.TargetID] = e.Detail
			case opChannelMemberAdd:
				added++
				logged.channel[e.TargetID] = true
			case opChannelMemberSkip:
				logged.channel[e.TargetID] = true
			case opOwnerAdd:
				logged.owner = true
			}
		}
		return nil
	}

	if legacy && !baseline[team.OwnerID] && !logged.owner {
		// The team was created without its owner. The owner goes in
		// first and by the single request, which is the one that can
		// carry the owner role.
		e := d.entry(lease, opOwnerAdd, "team/"+lease.Key, team.OwnerID, "")
		if present[team.OwnerID] {
			e.Detail = "added by an interrupted attempt"
		} else {
			if err := d.tenant.Wait(ctx); err != nil {
				return err
			}
			if err := g.AddTeamMember(ctx, team.TeamsID, team.OwnerID, true); err != nil && !graph.IsConflict(err) {
				return fmt.Errorf("adding the owner to team %q: %w", team.Name, err)
			}
		}
		if err := record([]store.LogEntry{e}); err != nil {
			return err
		}
	}

	// People an attempt that died added but did not get to record.
	var interrupted []store.LogEntry
	var missing []string
	for _, entraID := range ids {
		if present[entraID] {
			e := d.entry(lease, opMemberAdd, people[entraID], entraID, "")
			e.Detail = "added by an interrupted attempt"
			interrupted = append(interrupted, e)
		} else {
			missing = append(missing, entraID)
		}
	}
	if err := record(interrupted); err != nil {
		return err
	}
	for len(missing) > 0 {
		chunk := missing[:min(len(missing), graph.MaxMembersPerAdd)]
		missing = missing[len(chunk):]
		if err := d.tenant.Wait(ctx); err != nil {
			return err
		}
		results, err := g.AddTeamMembers(ctx, team.TeamsID, chunk)
		if err != nil {
			// Whether the request took effect is unknown. The next
			// attempt reads the membership and records whoever it added.
			return fmt.Errorf("adding members to team %q: %w", team.Name, err)
		}
		// Graph's answer is matched to the request by user ID, whatever
		// case it comes back in.
		asked := make(map[string]string, len(chunk))
		for _, id := range chunk {
			asked[strings.ToLower(id)] = id
		}
		var entries []store.LogEntry
		var failed error
		for _, res := range results {
			entraID, ok := asked[strings.ToLower(res.UserID)]
			if !ok {
				continue
			}
			delete(asked, strings.ToLower(res.UserID))
			e := d.entry(lease, opMemberAdd, people[entraID], entraID, "")
			switch {
			case res.Added():
			case res.Gone():
				// The account was mapped at plan time and has since
				// been deleted. One missing person must not keep the
				// whole team closed.
				e.Op, e.Outcome, e.Detail = opMemberSkip, "error", "the Microsoft 365 account no longer exists"
			case !res.Transient():
				// Graph's verdict on this one person: no licence, a
				// policy, an account it will not take. Asking again gets
				// the same answer, and one person must not keep the team
				// closed for good. They are recorded as not added, with
				// what Graph said, for an administrator to add by hand.
				e.Op, e.Outcome, e.Detail = opMemberSkip, "error", clip("Microsoft Teams refused to add them: "+res.Code+": "+res.Message)
			default:
				if failed == nil {
					failed = res.Err()
				}
				continue
			}
			entries = append(entries, e)
		}
		// Those who were added are recorded whatever happened to the
		// rest; the next attempt continues from here.
		if err := record(entries); err != nil {
			return err
		}
		if failed == nil && len(asked) > 0 {
			failed = fmt.Errorf("the answer from Microsoft Graph leaves out %d of the %d members in the request", len(asked), len(chunk))
		}
		if failed != nil {
			return fmt.Errorf("adding members to team %q: %w", team.Name, failed)
		}
	}

	// Private channels: each gets its own Slack members, who are members
	// of the team by now.
	for _, slackChannel := range private {
		// The cached mapping may predate the channel's creation.
		cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, slackChannel)
		if err != nil {
			return err
		}
		if cm.TeamsID == "" {
			return permanent(fmt.Errorf("private channel %q was never created in Teams, so it has no members to add", cm.SlackName))
		}
		if err := d.tenant.Wait(ctx); err != nil {
			return err
		}
		inNow := map[string]bool{}
		for m, err := range g.ChannelMembers(ctx, team.TeamsID, cm.TeamsID) {
			if err != nil {
				return fmt.Errorf("reading the members of private channel %q: %w", cm.TargetName, err)
			}
			inNow[m.UserID] = true
		}
		was, seen := cur.Channels[slackChannel]
		if !seen {
			if cur.Channels == nil {
				cur.Channels = map[string][]string{}
			}
			was = sortedIDs(inNow)
			cur.Channels[slackChannel] = was
			if err := saveCursor(); err != nil {
				return err
			}
		}
		before := idSet(was)
		own := inChannel[slackChannel]
		var want []string
		for id := range own {
			// Anyone who was in the channel before Taut added anybody is
			// left as they are; that includes the owner a channel was
			// created with, who must not be posted again as a plain
			// member (it could demote them). The team's owner is not left
			// out for being the owner: in a channel that already existed
			// they may not be a member at all.
			if !before[id] && !logged.channel[channelMemberTarget(cm.TeamsID, id)] {
				want = append(want, id)
			}
		}
		sort.Strings(want)
		considered += len(want)
		for _, entraID := range want {
			e := d.entry(lease, opChannelMemberAdd, slackChannel+"/"+own[entraID], channelMemberTarget(cm.TeamsID, entraID), "")
			switch reason, skipped := logged.skipped[entraID]; {
			case skipped:
				// Not in the team, so Teams would refuse them here too.
				e.Op, e.Outcome, e.Detail = opChannelMemberSkip, "error", "not added to the team: "+reason
			case inNow[entraID]:
				e.Detail = "added by an interrupted attempt"
			default:
				if err := d.tenant.Wait(ctx); err != nil {
					return err
				}
				switch err := g.AddChannelMember(ctx, team.TeamsID, cm.TeamsID, entraID, false); {
				case err == nil, graph.IsConflict(err):
				case graph.IsNotFound(err):
					// The channel was just read, so what is missing is
					// the account: deleted, or blocked from signing in.
					e.Op, e.Outcome, e.Detail = opChannelMemberSkip, "error", "the Microsoft 365 account no longer exists or is disabled"
				default:
					if definite(err) && !graph.IsNotTeamMember(err) {
						// Graph's verdict on this person in this channel.
						// Recorded, and the rest of the channel goes on.
						e.Op, e.Outcome, e.Detail = opChannelMemberSkip, "error", clip("Microsoft Teams refused to add them: "+describe(err))
						break
					}
					// Teams not yet seeing someone as a member of the
					// team moments after they were added, or anything
					// that may pass. People added so far are recorded;
					// the next attempt continues from here.
					return fmt.Errorf("adding members to private channel %q: %w", cm.TargetName, err)
				}
			}
			if err := record([]store.LogEntry{e}); err != nil {
				return err
			}
		}
	}

	stats := fmt.Sprintf(`{"added":%d,"unmapped":%d,"excluded":%d}`, added, len(unmapped), excluded)
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		return tx.CompleteBatch(ctx, b.Seq, "", considered, []byte(stats))
	})
	if err != nil {
		return err
	}
	d.cfg.Observer.Count(ctx, store.StageMembers, EventMemberAdded, int64(added))
	return nil
}

// memberEligible reports whether a Slack user may be made a member of a
// team: a full, current, human member of the workspace.
func memberEligible(u slack.User) bool {
	return !u.Deleted && !u.IsBot && !u.IsAppUser && !u.IsStranger && !u.IsGuest()
}
