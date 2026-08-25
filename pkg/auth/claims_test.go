package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/truvity/github-roster/pkg/config"
)

func TestRoleFromGroups(t *testing.T) {
	t.Parallel()

	roles := config.Roles{Viewer: "viewers", Operator: "operators"}

	assert.Equal(t, RoleOperator, RoleFromGroups([]string{"viewers", "operators"}, roles))
	assert.Equal(t, RoleViewer, RoleFromGroups([]string{"viewers"}, roles))
	assert.Equal(t, Role(""), RoleFromGroups([]string{"nobody"}, roles))
	assert.Equal(t, RoleViewer, RoleFromGroups(SplitGroups(" viewers , other "), roles))
}

func TestForwardToken(t *testing.T) {
	t.Parallel()

	// Authorization wins when present.
	assert.Equal(t, "Bearer abc", ForwardToken(func(k string) string {
		if k == "Authorization" {
			return "Bearer abc"
		}

		return "xyz"
	}))

	// Falls back to the gateway's access-token header, adding the scheme.
	assert.Equal(t, "Bearer xyz", ForwardToken(func(k string) string {
		if k == "X-Auth-Request-Access-Token" {
			return "xyz"
		}

		return ""
	}))

	// Neither present → empty.
	assert.Equal(t, "", ForwardToken(func(string) string { return "" }))
}

func TestRoleFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		values []string
		want   Role
	}{
		{"no claims at all", nil, RoleNone},
		{"an unrelated group", []string{"everyone"}, RoleNone},
		{"viewer", []string{"viewers"}, RoleViewer},
		{"operator", []string{"operators"}, RoleOperator},
		// Someone in both groups is an operator. The alternative — order
		// deciding it — would make a person's permissions depend on how
		// their provider happened to sort the claim.
		{"both, operator listed last", []string{"viewers", "operators"}, RoleOperator},
		{"both, operator listed first", []string{"operators", "viewers"}, RoleOperator},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, roleFor(tc.values, "viewers", "operators"))
		})
	}
}

func TestRolePermissions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		role     Role
		canView  bool
		canWrite bool
	}{
		{RoleNone, false, false},
		{RoleViewer, true, false},
		{RoleOperator, true, true},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.canView, tc.role.CanView(), "CanView for %q", tc.role)
		assert.Equal(t, tc.canWrite, tc.role.CanOperate(), "CanOperate for %q", tc.role)
	}
}
