package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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

// Providers disagree about whether a single group is a string or a
// one-element array, and a decoded token yields []any where hand-built one
// yields []string. Getting this wrong silently strips everyone's role.
func TestStringsFrom(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  any
		want []string
	}{
		{"absent", nil, nil},
		{"single string", "a", []string{"a"}},
		{"string slice", []string{"a", "b"}, []string{"a", "b"}},
		{"decoded JSON array", []any{"a", "b"}, []string{"a", "b"}},
		{"array with a non-string", []any{"a", 42}, []string{"a"}},
		{"empty array", []any{}, []string{}},
		{"a number is not a role", 42, nil},
		{"an object is not a role", map[string]any{"a": 1}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, stringsFrom(tc.raw))
		})
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"absent", "", "", false},
		{"canonical", "Bearer abc.def.ghi", "abc.def.ghi", true},
		// RFC 7235 makes the scheme case-insensitive, and proxies have been
		// known to normalize it.
		{"lowercase scheme", "bearer abc.def.ghi", "abc.def.ghi", true},
		{"mixed-case scheme", "BeArEr abc.def.ghi", "abc.def.ghi", true},
		{"trailing space", "Bearer abc.def.ghi ", "abc.def.ghi", true},
		{"scheme but no token", "Bearer ", "", false},
		{"a different scheme", "Basic dXNlcjpwYXNz", "", false},
		// A bare token is not a credential this service accepts: taking it
		// would mean guessing at a format the gateway never sends.
		{"no scheme", "abc.def.ghi", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := bearerToken(tc.header)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.ok, ok)
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
