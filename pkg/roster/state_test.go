package roster

import "testing"

func TestMembershipState(t *testing.T) {
	tests := []struct {
		name string
		m    Membership
		want MembershipState
	}{
		{
			name: "invited",
			m:    Membership{Live: true, Member: true, InvitationPending: true, DesiredTeams: []string{"team-a"}},
			want: StateInvited,
		},
		{
			name: "leaving: suspended but still a member",
			m:    Membership{Live: false, Member: true, Teams: []string{"team-a"}, DesiredTeams: []string{"team-a"}},
			want: StateLeaving,
		},
		{
			name: "synced: member, live, teams match",
			m:    Membership{Live: true, Member: true, Teams: []string{"team-a", "team-b"}, DesiredTeams: []string{"team-a", "team-b"}},
			want: StateSynced,
		},
		{
			name: "pending: member but teams differ",
			m:    Membership{Live: true, Member: true, Teams: []string{"team-a"}, DesiredTeams: []string{"team-a", "team-b"}},
			want: StatePending,
		},
		{
			name: "pending: live, desired, not yet a member",
			m:    Membership{Live: true, Member: false, DesiredTeams: []string{"team-a"}},
			want: StatePending,
		},
		{
			name: "none: not desired here",
			m:    Membership{Live: true, Member: false},
			want: StateNone,
		},
		{
			name: "none: suspended and not a member",
			m:    Membership{Live: false, Member: false, DesiredTeams: []string{"team-a"}},
			want: StateNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := membershipState(tc.m); got != tc.want {
				t.Fatalf("membershipState(%+v) = %q, want %q", tc.m, got, tc.want)
			}
		})
	}
}

func TestTeamsEqual(t *testing.T) {
	if !teamsEqual([]string{"a", "b"}, []string{"a", "b"}) {
		t.Fatal("equal lists must compare equal")
	}
	if teamsEqual([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("different lengths must differ")
	}
	if teamsEqual([]string{"a", "c"}, []string{"a", "b"}) {
		t.Fatal("different members must differ")
	}
	if !teamsEqual(nil, nil) {
		t.Fatal("both empty must compare equal")
	}
}
