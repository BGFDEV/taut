package slack

import "sort"

// WorkspaceID returns the ID of the workspace an export's users.json
// belongs to: the team_id most of its users carry. Slack Connect users from
// other organizations carry their own workspace's ID and are not counted.
// It returns "" when no user has a team_id, which is how an export that
// was edited or trimmed can look.
//
// An export has no field that names its workspace, so this is the best
// statement of identity it makes. Ties go to the smallest ID, so the answer
// does not depend on the order of users.json.
func WorkspaceID(users []User) string {
	counts := map[string]int{}
	for _, u := range users {
		if u.TeamID == "" || u.IsStranger {
			continue
		}
		counts[u.TeamID]++
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// WorkspaceMatch is the outcome of comparing an export with the workspace
// earlier passes of a migration were made from.
type WorkspaceMatch struct {
	// Same is true when the export is taken to be of the same workspace.
	Same bool
	// Shared is how many of the earlier conversations the export lists.
	Shared int
	// Earlier is how many conversations the earlier passes knew.
	Earlier int
}

// SameWorkspace decides whether an export is of the workspace that earlier
// passes migrated. earlierID and exportID are WorkspaceID of each side;
// earlier and exported are the Slack IDs of the conversations each knows.
//
// Two signals are used, because neither is always there. Conversation IDs
// are unique across Slack, so one conversation in common settles it. With
// none in common the workspace IDs decide, when both sides have one; an
// export that shares no conversation and cannot be shown to have the same
// ID is refused. Earlier passes that recorded no conversation at all have
// nothing to contradict.
func SameWorkspace(earlierID string, earlier []string, exportID string, exported []string) WorkspaceMatch {
	in := make(map[string]bool, len(exported))
	for _, id := range exported {
		in[id] = true
	}
	m := WorkspaceMatch{Earlier: len(earlier)}
	for _, id := range earlier {
		if in[id] {
			m.Shared++
		}
	}
	switch {
	case m.Shared > 0, m.Earlier == 0:
		m.Same = true
	default:
		m.Same = earlierID != "" && earlierID == exportID
	}
	return m
}
