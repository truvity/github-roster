package server

import (
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"

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
	}

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
	}

	return d.Renderer.Render(c, fiber.StatusOK, "structure", ui.Page{
		Title:  "Structure",
		Nav:    "structure",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}

func (d *Deps) handleMapping(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "mapping", ui.Page{
		Title:  "Mapping",
		Nav:    "mapping",
		AuthOn: d.Auth.Enabled(),
	})
}

func (d *Deps) handleAudit(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "audit", ui.Page{
		Title:  "Audit",
		Nav:    "audit",
		AuthOn: d.Auth.Enabled(),
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
