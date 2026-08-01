package server

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/roster"
	"github.com/truvity/github-roster/pkg/ui"
)

// structureData is what the structure page renders. It is a view model, not
// the config type: the page shows teams in a stable order, which a map
// cannot promise, and ranging a map would reshuffle the page on every load.
type structureData struct {
	Orgs []structureOrg
	// Sources carries each directory's health. Shown rather than hidden:
	// an operator reading a stale directory needs to know, because the
	// alternative is confidently acting on it.
	Sources []directory.Status
	// Roster is the join, when the read layers are wired. Nil in tests
	// that only exercise routing.
	Roster *roster.Roster
	// RosterError explains why the join is missing, rather than rendering
	// an empty table that reads as "nobody works here".
	RosterError string
	// GitHub says how old each organization's GitHub answer is — the
	// cache must never pass a stale read off as live.
	GitHub []githubAge
	// SourceNames and OrgNames are the table's column groups, in stable
	// order: one identity column per directory (IdP), one standing
	// column per organization.
	SourceNames []string
	OrgNames    []string
}

type githubAge struct {
	Org string
	Age string
}

type structureOrg struct {
	Name  string
	Teams []structureTeam
}

type structureTeam struct {
	Name   string
	Groups []string
	Pinned bool
}

func (d *Deps) handleStructure(c fiber.Ctx) error {
	data := structureData{Orgs: make([]structureOrg, 0, len(d.Config.Orgs))}

	for i := range d.Config.Orgs {
		org := &d.Config.Orgs[i]
		view := structureOrg{Name: org.Name, Teams: make([]structureTeam, 0, len(org.Teams))}

		for _, name := range sortedTeamNames(org.Teams) {
			team := org.Teams[name]
			view.Teams = append(view.Teams, structureTeam{
				Name:   name,
				Groups: team.Groups,
				Pinned: team.Pinned,
			})
		}

		data.Orgs = append(data.Orgs, view)
	}

	if d.Directories != nil {
		data.Sources = d.Directories.Statuses()

		for _, s := range data.Sources {
			data.SourceNames = append(data.SourceNames, s.Source)
		}

		sort.Strings(data.SourceNames)
	}

	for i := range d.Config.Orgs {
		data.OrgNames = append(data.OrgNames, d.Config.Orgs[i].Name)
	}

	sort.Strings(data.OrgNames)

	if d.Mapping != nil {
		joined, err := d.buildRoster(c.Context())
		if err != nil {
			// Render the page with the failure named. A blank people table
			// looks like an answer; this looks like the problem it is.
			d.Logger.ErrorContext(c.Context(), "structure page: join failed", "error", err)
			data.RosterError = err.Error()
		} else {
			data.Roster = joined
		}

		// A second Read against the cache is free; it exists to render
		// "as of" honestly.
		readAt := d.githubReadAt(c.Context())
		for _, name := range slices.Sorted(maps.Keys(readAt)) {
			data.GitHub = append(data.GitHub, githubAge{
				Org: name,
				Age: time.Since(readAt[name]).Round(time.Second).String(),
			})
		}
	}

	return d.Renderer.Render(c, fiber.StatusOK, "structure", ui.Page{
		Title:  "Structure",
		Nav:    "structure",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}

// auditData is the audit log page.
type auditData struct {
	Records []audit.Record
	Org     string
	Orgs    []string
	Error   string
}

func (d *Deps) handleAudit(c fiber.Ctx) error {
	data := auditData{Orgs: d.orgNames(), Org: c.Query("org")}

	if d.Audit == nil {
		data.Error = "no audit sink is configured in this deployment"
	} else if records, err := d.Audit.List(c.Context(), data.Org, audit.DefaultLimit); err != nil {
		// Say so rather than render an empty table: "no records" and
		// "could not read the records" look identical and mean opposite
		// things.
		data.Error = err.Error()
	} else {
		data.Records = records
	}

	return d.Renderer.Render(c, fiber.StatusOK, "audit", ui.Page{
		Title:  "Audit",
		Nav:    "audit",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}

// requireOperator refuses a viewer reaching a page that changes something.
// Applied per route, because "which pages write" is exactly the thing that
// must be stated explicitly rather than inherited.
func requireOperator(c fiber.Ctx) error {
	identity, ok := auth.From(c)
	if !ok || !identity.Role.CanOperate() {
		return fiber.NewError(fiber.StatusForbidden, "operator role required")
	}

	return c.Next()
}

// wantsHTML reports whether the caller is a browser. Used to decide whether
// an error is a page or a JSON body.
func wantsHTML(c fiber.Ctx) bool {
	return strings.Contains(c.Get(fiber.HeaderAccept), fiber.MIMETextHTML)
}

func sortedTeamNames(teams map[string]config.Team) []string {
	names := make([]string, 0, len(teams))
	for name := range teams {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
