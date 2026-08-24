package auth

import (
	"strings"

	"github.com/lestrrat-go/jwx/v4/jwt"

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

// identityFrom reads the claims this service cares about.
//
// Claims are read one at a time rather than into a struct, because the roles
// claim is named by configuration and because providers disagree about
// whether a single group arrives as a string or a one-element array.
func (v *verifier) identityFrom(token jwt.Token) Identity {
	subject, _ := token.Subject()

	return Identity{
		Subject: subject,
		Name:    stringClaim(token, "name"),
		Email:   stringClaim(token, "email"),
		Role:    roleFor(claimValues(token, v.rolesClaim), v.roles.Viewer, v.roles.Operator),
	}
}

func stringClaim(token jwt.Token, name string) string {
	raw, ok := token.Field(name)
	if !ok {
		return ""
	}

	value, _ := raw.(string)

	return value
}

// claimValues reads a claim that may be a string or an array of strings, and
// returns nothing for anything else. Getting this wrong silently strips
// everyone's role, so both shapes are handled explicitly.
func claimValues(token jwt.Token, name string) []string {
	raw, ok := token.Field(name)
	if !ok {
		return nil
	}

	return stringsFrom(raw)
}

// stringsFrom normalizes the shapes a roles claim arrives in. A decoded JWT
// yields []any for arrays, but a caller constructing a token by hand — the
// tests do — produces []string.
func stringsFrom(raw any) []string {
	switch value := raw.(type) {
	case string:
		return []string{value}
	case []string:
		return value
	case []any:
		values := make([]string, 0, len(value))

		for _, item := range value {
			if s, ok := item.(string); ok {
				values = append(values, s)
			}
		}

		return values
	default:
		return nil
	}
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
