package directory_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	directoryv1 "github.com/truvity/google-group-sync/gen/directory/v1"
	"github.com/truvity/google-group-sync/gen/directory/v1/directoryv1connect"

	"github.com/truvity/github-roster/pkg/directory"
)

// fakeDirectory is an in-memory DirectoryService for the resolver test.
type fakeDirectory struct {
	directoryv1connect.UnimplementedDirectoryServiceHandler
	groups   map[string][]string // group -> members (missing = not found)
	accounts map[string]*directoryv1.Account
}

func (f *fakeDirectory) GetGroup(_ context.Context, req *connect.Request[directoryv1.GetGroupRequest]) (*connect.Response[directoryv1.GetGroupResponse], error) {
	members, ok := f.groups[req.Msg.GetEmail()]
	if !ok {
		return connect.NewResponse(&directoryv1.GetGroupResponse{Found: false}), nil
	}
	return connect.NewResponse(&directoryv1.GetGroupResponse{
		Found: true,
		Group: &directoryv1.Group{Email: req.Msg.GetEmail(), Members: members},
	}), nil
}

func (f *fakeDirectory) ResolveAccounts(_ context.Context, req *connect.Request[directoryv1.ResolveAccountsRequest]) (*connect.Response[directoryv1.ResolveAccountsResponse], error) {
	var out []*directoryv1.Account
	for _, e := range req.Msg.GetEmails() {
		if a, ok := f.accounts[e]; ok {
			out = append(out, a)
		} else {
			out = append(out, &directoryv1.Account{Email: e, InDomain: false})
		}
	}
	return connect.NewResponse(&directoryv1.ResolveAccountsResponse{Accounts: out}), nil
}

func TestResolverFetch(t *testing.T) {
	fake := &fakeDirectory{
		groups: map[string][]string{
			"team-eng@acme.example": {"dana@acme.example", "partner@other.example"},
			// team-missing@acme.example intentionally absent
		},
		accounts: map[string]*directoryv1.Account{
			"dana@acme.example":     {Email: "dana@acme.example", InDomain: true, Found: true, Live: true, GivenName: "Dana", FamilyName: "Okafor"},
			"partner@other.example": {Email: "partner@other.example", InDomain: false},
		},
	}

	_, handler := directoryv1connect.NewDirectoryServiceHandler(fake)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	src, err := directory.NewResolver(directory.ResolverConfig{
		Name:     "acme",
		Endpoint: srv.URL,
		Domains:  []string{"acme.example"},
		Groups:   []string{"team-eng@acme.example", "team-missing@acme.example"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Present group's members are recorded; missing group is absent, not fatal.
	if got := snap.Groups["team-eng@acme.example"]; len(got) != 2 {
		t.Fatalf("group members = %v, want 2", got)
	}
	if len(snap.AbsentGroups) != 1 || snap.AbsentGroups[0] != "team-missing@acme.example" {
		t.Fatalf("absent groups = %v", snap.AbsentGroups)
	}

	// Only the in-domain account becomes a User, with its name and liveness.
	if len(snap.Users) != 1 {
		t.Fatalf("users = %+v, want only the in-domain account", snap.Users)
	}
	u := snap.Users[0]
	if u.Email != "dana@acme.example" || u.Name != "Dana Okafor" || !u.Live {
		t.Fatalf("unexpected user: %+v", u)
	}
}
