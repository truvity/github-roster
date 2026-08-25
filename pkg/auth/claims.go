package auth

import (
	"strings"

	"github.com/truvity/github-roster/pkg/config"
)

// RoleFromGroups picks the role a set of gateway-forwarded groups grants,
// using the same operator-wins rule as token verification. It backs GetMe,
// which reads the oauth2-proxy X-Auth-Request-Groups header rather than the
// raw token.
func RoleFromGroups(groups []string, roles config.Roles) Role {
	return roleFor(groups, roles.Viewer, roles.Operator)
}

// SplitGroups parses the comma-separated X-Auth-Request-Groups header value.
func SplitGroups(header string) []string {
	var out []string

	for _, g := range strings.Split(header, ",") {
		if t := strings.TrimSpace(g); t != "" {
			out = append(out, t)
		}
	}

	return out
}

// roleFor picks the strongest role the caller's claim values grant.
//
// Operator wins over viewer, so someone in both groups is an operator. The
// alternative — first match wins — would make a person's permissions depend
// on how their provider happened to sort a claim.
func roleFor(values []string, viewer, operator string) Role {
	var role Role

	for _, v := range values {
		switch v {
		case operator:
			return RoleOperator
		case viewer:
			role = RoleViewer
		}
	}

	return role
}
