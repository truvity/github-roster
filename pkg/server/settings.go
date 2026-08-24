package server

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/ui"
)

// settingsData drives the Settings page. Read-only for now: directories,
// organizations and teams come from the git-delivered config document, so
// they render as managed-in-git. The store + editing (and the GitHub App
// manifest flow) are the follow-on slices of this stream.
type settingsData struct {
	Sources []config.Source `json:"sources"`
	// StoreSources are operator-added directories (editable/deletable here),
	// distinct from the git-declared Sources above.
	StoreSources []config.Source `json:"storeSources,omitempty"`
	Orgs         []settingsOrg   `json:"orgs"`
	// StoreOrgs are operator-added organizations staged in the config store,
	// shown separately from the git-declared Orgs. Display only for now — the
	// reconciler still runs the git orgs; a store org is born disabled.
	StoreOrgs []settingsOrg `json:"storeOrgs,omitempty"`
	Flash     string        `json:"-"`
}

type settingsOrg struct {
	Name             string `json:"name"`
	Company          string `json:"company"`
	MinAdmins        int    `json:"minAdmins"`
	ReconcileEnabled bool   `json:"reconcileEnabled"`
	// Provenance is set for store orgs ("manual"|"roster"); empty for git.
	Provenance string         `json:"provenance,omitempty"`
	Teams      []settingsTeam `json:"teams"`
}

type settingsTeam struct {
	Name    string   `json:"name"`
	Groups  []string `json:"groups,omitempty"`
	Members []string `json:"members,omitempty"`
	Pinned  bool     `json:"pinned,omitempty"`
}

func (d *Deps) handleSettings(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "settings", ui.Page{
		Title:  "Settings",
		Nav:    "settings",
		AuthOn: d.Auth.Enabled(),
		Data:   d.settingsWithFlash(c),
	})
}

// buildSettings assembles the directories, organizations and teams view
// from the configuration — shared by the SSR page and the JSON API.
func (d *Deps) buildSettings(ctx context.Context) settingsData {
	data := settingsData{Sources: d.Config.Sources}

	// Operator-added directories, shown separately and editable. Filter
	// them out of the git list so a directory that survived into cfg via a
	// prior restart is not shown twice.
	if d.DirStore != nil {
		if stored, err := d.DirStore.List(ctx); err == nil && len(stored) > 0 {
			data.StoreSources = stored

			storeName := make(map[string]bool, len(stored))
			for i := range stored {
				storeName[stored[i].Name] = true
			}

			var git []config.Source
			for i := range d.Config.Sources {
				if !storeName[d.Config.Sources[i].Name] {
					git = append(git, d.Config.Sources[i])
				}
			}

			data.Sources = git
		}
	}

	data.Orgs = toSettingsOrgs(d.Config.Orgs)

	// Operator-added organizations staged in the store, shown separately.
	// Git wins by name (a store org shadowed by git is not shown twice).
	// Display only: the reconciler is unchanged — these are staged, born
	// disabled, and do not run until a later slice wires them behind the gate.
	if d.OrgStore != nil {
		if stored, err := d.OrgStore.ListOrgs(ctx); err == nil && len(stored) > 0 {
			gitNames := make(map[string]bool, len(d.Config.Orgs))
			for i := range d.Config.Orgs {
				gitNames[strings.ToLower(d.Config.Orgs[i].Name)] = true
			}

			var storeOnly []config.Org
			for i := range stored {
				if !gitNames[strings.ToLower(stored[i].Name)] {
					storeOnly = append(storeOnly, stored[i])
				}
			}

			data.StoreOrgs = toSettingsOrgs(storeOnly)
		}
	}

	return data
}

// toSettingsOrgs maps config orgs to the settings view shape (teams sorted by
// name), shared by the git and store lists.
func toSettingsOrgs(orgs []config.Org) []settingsOrg {
	out := make([]settingsOrg, 0, len(orgs))

	for i := range orgs {
		o := &orgs[i]
		so := settingsOrg{
			Name:             o.Name,
			Company:          o.Company,
			MinAdmins:        o.MinAdmins,
			ReconcileEnabled: o.ReconcileEnabled,
			Provenance:       o.Provenance,
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

		out = append(out, so)
	}

	return out
}

// handleSettingsAddDirectory writes an operator-added resolver-backed
// directory to the config store. The broker's reconcile loop picks it up on
// its next pass; it takes effect there without a restart.
func (d *Deps) handleSettingsAddDirectory(c fiber.Ctx) error {
	if d.DirStore == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "no config store configured")
	}

	name := strings.TrimSpace(formValue(c, "name"))
	endpoint := strings.TrimSpace(formValue(c, "endpoint"))
	probeGroup := strings.TrimSpace(formValue(c, "probeGroup"))

	var domains []string
	for _, dm := range strings.Split(formValue(c, "domains"), ",") {
		if t := strings.TrimSpace(dm); t != "" {
			domains = append(domains, t)
		}
	}

	if name == "" || endpoint == "" || len(domains) == 0 {
		return c.Redirect().To("/settings?flash=" + url.QueryEscape("a name, an endpoint and at least one domain are required"))
	}

	src := config.Source{Name: name, Endpoint: endpoint, Domains: domains, ProbeGroup: probeGroup}
	if err := d.DirStore.Put(c.Context(), src); err != nil {
		return c.Redirect().To("/settings?flash=" + url.QueryEscape("could not add directory: "+err.Error()))
	}

	return c.Redirect().To("/settings?flash=" + url.QueryEscape("added directory "+name+" — the reconcile loop will use it on its next pass"))
}

// handleSettingsDeleteDirectory removes an operator-added directory.
func (d *Deps) handleSettingsDeleteDirectory(c fiber.Ctx) error {
	if d.DirStore == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "no config store configured")
	}

	name := strings.TrimSpace(formValue(c, "name"))
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}

	if err := d.DirStore.Delete(c.Context(), name); err != nil {
		return c.Redirect().To("/settings?flash=" + url.QueryEscape("could not delete directory: "+err.Error()))
	}

	return c.Redirect().To("/settings?flash=deleted directory " + url.QueryEscape(name))
}

// settingsWithFlash renders the settings view with a one-shot flash from
// the query string (post-redirect-get).
func (d *Deps) settingsWithFlash(c fiber.Ctx) settingsData {
	data := d.buildSettings(c.Context())
	data.Flash = c.Query("flash")

	return data
}
