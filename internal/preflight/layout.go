package preflight

import (
	"fmt"
	"strings"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/ui/sanitize"
)

// Assignment places one Slack channel in the plan.
type Assignment struct {
	Channel slack.Channel
	// Team indexes PlanLayout.Teams; Slot indexes that team's Channels.
	Team int
	Slot int
}

// PlanLayout is the Slack-to-Teams mapping the config file implies for an
// export: which channels migrate, into which team, under which name.
//
// It is computed in exactly one place. `taut preflight` reports it and the plan
// stage of a run executes it, so what the preflight shows is what the
// migration does.
type PlanLayout struct {
	Teams       []TeamPlan
	Skipped     []SkippedChannel
	Assignments []Assignment
	Blockers    []Finding
	Warnings    []Finding
}

func (l *PlanLayout) block(code, msg, next string) {
	l.Blockers = append(l.Blockers, Finding{Code: code, Message: msg, Next: next})
}

func (l *PlanLayout) warn(code, msg, next string) {
	l.Warnings = append(l.Warnings, Finding{Code: code, Message: msg, Next: next})
}

// Layout applies the config file's channel selection and Teams naming rules to
// the export's channels. existingTeams are display names already present in
// the tenant, used only to flag collisions.
func Layout(m *config.Config, channels []slack.Channel, existingTeams []string) *PlanLayout {
	l := &PlanLayout{Skipped: []SkippedChannel{}}
	existing := map[string]bool{}
	for _, n := range existingTeams {
		existing[strings.ToLower(n)] = true
	}
	l.Teams = make([]TeamPlan, len(m.Target.Teams))
	usedNames := make([]map[string]bool, len(m.Target.Teams))
	for i, t := range m.Target.Teams {
		l.Teams[i] = TeamPlan{Name: t.Name, Owner: t.Owner, Exists: existing[strings.ToLower(t.Name)], Channels: []ChannelPlan{}}
		usedNames[i] = map[string]bool{}
		switch {
		case !l.Teams[i].Exists:
		case m.Load.Strategy == config.StrategyLegacy:
			// The legacy strategy imports only into a team it created in
			// migration mode; an existing team is not in it and cannot be
			// put into it.
			l.block("team.exists_legacy", fmt.Sprintf("A team named %q already exists in the tenant, and the legacy load strategy cannot import into an existing team.", t.Name),
				"Choose a name no team has yet (target.teams[].name), or delete the existing team in Teams. In the global cloud, load.strategy: modern can import into an existing team.")
		default:
			l.warn("team.exists", fmt.Sprintf("A team named %q already exists in the tenant.", t.Name),
				"Taut will import into the existing team. Rename the team in the config file if that is not what you want.")
		}
	}

	dmCount := 0
	for _, ch := range channels {
		kind := string(ch.Kind)
		if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM {
			dmCount++
			continue
		}
		sel := m.SelectingTeams(ch.Name, kind)
		if sanitize.HasControl(ch.Name) || sanitize.HasControl(ch.ID) {
			// Slack accepts no control character in a channel name or ID,
			// so this is not something a person typed. The name is used
			// as it is for matching; only what is shown is cleaned.
			msg := fmt.Sprintf("The channel shown as #%s (%s) has control characters in its name or ID.",
				sanitize.Text(ch.Name), sanitize.Text(ch.ID))
			if len(sel) > 0 {
				l.block("export.channel_control_chars", msg,
					"Slack does not allow these, so the export was altered or is damaged. Export the workspace again, or leave the channel out with an exclude pattern in the config file.")
			} else {
				l.warn("export.channel_control_chars", msg+" It is not migrating.",
					"Slack does not allow these, so the export was altered or is damaged. Export the workspace again if you did not expect this.")
			}
		}
		switch len(sel) {
		case 0:
			reason := "not selected by any team's include/exclude patterns"
			if ch.Kind == slack.KindPrivate && !selectsKind(m, config.ChannelPrivate) {
				reason = "private channels are not included"
			}
			l.Skipped = append(l.Skipped, SkippedChannel{SlackName: ch.Name, Kind: kind, Reason: reason})
			continue
		default:
			if len(sel) > 1 {
				l.block("plan.channel_ambiguous",
					fmt.Sprintf("#%s is selected by %d teams (%s).", ch.Name, len(sel), teamNames(m, sel)),
					"Adjust the include/exclude patterns so each channel belongs to exactly one team.")
			}
		}
		ti := sel[0]
		spec := m.Target.Teams[ti]

		target, reason := TeamsChannelName(spec.TargetName(ch.Name))
		if ch.IsGeneral {
			// Every team already has a General channel; #general maps
			// onto it rather than creating a second one.
			target, reason = teamsGeneral, ""
		} else if strings.EqualFold(target, teamsGeneral) {
			target, reason = target+"-2", "\"General\" is taken by the team's default channel"
		}
		base := target
		for n := 2; usedNames[ti][strings.ToLower(target)]; n++ {
			target = fmt.Sprintf("%s-%d", base, n)
			reason = "another channel already maps to this name"
		}
		usedNames[ti][strings.ToLower(target)] = true

		cp := ChannelPlan{
			SlackID: ch.ID, SlackName: ch.Name, Kind: kind, TargetName: target,
			Renamed: target != ch.Name && !ch.IsGeneral, RenameReason: reason, Archived: ch.IsArchived,
		}
		if cp.Renamed && reason == "" {
			cp.RenameReason = "renamed in the config file"
		}
		l.Teams[ti].Channels = append(l.Teams[ti].Channels, cp)
		l.Assignments = append(l.Assignments, Assignment{Channel: ch, Team: ti, Slot: len(l.Teams[ti].Channels) - 1})
	}
	// With direct messages on, each conversation is decided on its own
	// (DirectMessages) and reported in its own section.
	if dmCount > 0 && !m.Target.DirectMessages.Enabled {
		l.Skipped = append(l.Skipped, SkippedChannel{SlackName: fmt.Sprintf("%d direct-message conversations", dmCount), Kind: "dm",
			Reason: "direct messages are turned off"})
	}
	return l
}
