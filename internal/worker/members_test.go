package worker

import (
	"testing"

	"github.com/bgfdev/taut/internal/slack"
)

// Team membership is access. Mapping an account is enough to attribute a
// message to it, but not to let its owner into a team.
func TestMemberEligible(t *testing.T) {
	for name, tc := range map[string]struct {
		user slack.User
		want bool
	}{
		"member":               {slack.User{ID: "U1"}, true},
		"deactivated":          {slack.User{ID: "U2", Deleted: true}, false},
		"bot":                  {slack.User{ID: "U3", IsBot: true}, false},
		"app user":             {slack.User{ID: "U4", IsAppUser: true}, false},
		"multi-channel guest":  {slack.User{ID: "U5", IsRestricted: true}, false},
		"single-channel guest": {slack.User{ID: "U6", IsRestricted: true, IsUltraRestricted: true}, false},
		"slack connect":        {slack.User{ID: "U7", IsStranger: true}, false},
	} {
		if got := memberEligible(tc.user); got != tc.want {
			t.Errorf("%s: eligible = %v, want %v", name, got, tc.want)
		}
	}
}
