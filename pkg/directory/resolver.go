package directory

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	directoryv1 "github.com/truvity/google-group-sync/gen/directory/v1"
	"github.com/truvity/google-group-sync/gen/directory/v1/directoryv1connect"
)

// Resolver is a [Source] backed by a DirectoryService endpoint
// (google-group-sync speaking ConnectRPC) instead of reading a directory
// directly. The roster then holds no directory credential — only the
// endpoint. Opt-in per source: a source with an Endpoint uses this; one
// without keeps the in-process Google reader.
//
// Snapshot semantics differ from the Google reader in one deliberate way:
// Users are the accounts found among the MAPPED GROUPS' members, not every
// account in the domain. The continuous model only cares about people in a
// team-backing group (NEW = live and in such a group), and DirectoryService
// exposes no whole-directory enumeration by design.
type Resolver struct {
	name       string
	domains    []string
	groups     []string // mapped groups this source owns
	probeGroup string
	client     directoryv1connect.DirectoryServiceClient
}

// ResolverConfig configures a DirectoryService-backed source.
type ResolverConfig struct {
	// Name is the source's configured name.
	Name string
	// Endpoint is the DirectoryService base URL (the ggs Service).
	Endpoint string
	// Domains this source is responsible for.
	Domains []string
	// Groups are the mapped groups this source owns — the only ones it
	// asks about, mirroring the Google reader's scoping.
	Groups []string
	// ProbeGroup is the health canary; optional.
	ProbeGroup string
	// HTTPClient is optional; defaults to a 30s client.
	HTTPClient connect.HTTPClient
}

// NewResolver builds a DirectoryService-backed source.
func NewResolver(cfg ResolverConfig) (*Resolver, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("resolver source: name is required")
	}

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("resolver source %q: endpoint is required", cfg.Name)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &Resolver{
		name:       cfg.Name,
		domains:    cfg.Domains,
		groups:     cfg.Groups,
		probeGroup: cfg.ProbeGroup,
		client:     directoryv1connect.NewDirectoryServiceClient(httpClient, cfg.Endpoint),
	}, nil
}

// Name is the source's configured name.
func (r *Resolver) Name() string { return r.name }

// Fetch builds a snapshot from the DirectoryService: the mapped groups and
// the accounts among their members. A read failure returns an error rather
// than a partial snapshot — the same contract the Google reader keeps.
func (r *Resolver) Fetch(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{
		Source:    r.name,
		Groups:    make(map[string][]string, len(r.groups)),
		FetchedAt: time.Now().UTC(),
	}

	members := map[string]struct{}{}

	for _, group := range r.groups {
		resp, err := r.client.GetGroup(ctx, connect.NewRequest(&directoryv1.GetGroupRequest{Email: group}))
		if err != nil {
			return nil, fmt.Errorf("source %q: get group %q: %w", r.name, group, err)
		}

		// An absent group fails safe per group (like the Google reader's
		// probe-group path), not the whole fetch.
		if !resp.Msg.GetFound() {
			snap.AbsentGroups = append(snap.AbsentGroups, group)

			continue
		}

		g := resp.Msg.GetGroup()
		snap.Groups[group] = g.GetMembers()

		for _, m := range g.GetMembers() {
			members[strings.ToLower(m)] = struct{}{}
		}
	}

	emails := make([]string, 0, len(members))
	for e := range members {
		emails = append(emails, e)
	}

	sort.Strings(emails)

	if len(emails) > 0 {
		resp, err := r.client.ResolveAccounts(ctx, connect.NewRequest(&directoryv1.ResolveAccountsRequest{Emails: emails}))
		if err != nil {
			return nil, fmt.Errorf("source %q: resolve accounts: %w", r.name, err)
		}

		for _, a := range resp.Msg.GetAccounts() {
			// Only accounts this directory vouches for become Users: an
			// out-of-domain member (a partner) is somebody else's account
			// and resolves from their home directory.
			if !a.GetInDomain() || !a.GetFound() {
				continue
			}

			snap.Users = append(snap.Users, User{
				Name:  fullName(a.GetGivenName(), a.GetFamilyName(), a.GetEmail()),
				Email: a.GetEmail(),
				Live:  a.GetLive(),
			})
		}
	}

	return snap, nil
}

// fullName builds the "First Last" join key, falling back to the local part
// of the address when the directory supplies no name (the user-read scope
// may be absent — an operator then types the name on Add).
func fullName(given, family, email string) string {
	name := strings.TrimSpace(given + " " + family)
	if name != "" {
		return name
	}

	if at := strings.Index(email, "@"); at > 0 {
		return email[:at]
	}

	return email
}
