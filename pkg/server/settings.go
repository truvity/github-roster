package server

import (
	"sort"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/ui"
)

// settingsData drives the Settings page. Read-only for now: directories,
// organizations and teams come from the git-delivered config document, so
// they render as managed-in-git. The store + editing (and the GitHub App
// manifest flow) are the follow-on slices of this stream.
type settingsData struct {
	Sources []config.Source
	Orgs    []settingsOrg
}

type settingsOrg struct {
	Name             string
	Company          string
	MinAdmins        int
	ReconcileEnabled bool
	Teams            []settingsTeam
}

type settingsTeam struct {
	Name    string
	Groups  []string
	Members []string
	Pinned  bool
}

func (d *Deps) handleSettings(c fiber.Ctx) error {
	data := settingsData{Sources: d.Config.Sources}

	for i := range d.Config.Orgs {
		o := &d.Config.Orgs[i]
		so := settingsOrg{
			Name:             o.Name,
			Company:          o.Company,
			MinAdmins:        o.MinAdmins,
			ReconcileEnabled: o.ReconcileEnabled,
		}

		names := make([]string, 0, len(o.Teams))
		for name := range o.Teams {
			names = append(names, name)
		}

		sort.Strings(names)

		for _, name := range names {
			t := o.Teams[name]
			so.Teams = append(so.Teams, settingsTeam{
				Name:    name,
				Groups:  t.Groups,
				Members: t.Members,
				Pinned:  t.Pinned,
			})
		}

		data.Orgs = append(data.Orgs, so)
	}

	return d.Renderer.Render(c, fiber.StatusOK, "settings", ui.Page{
		Title:  "Settings",
		Nav:    "settings",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}
