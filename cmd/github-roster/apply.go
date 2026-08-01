package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/reconciler"
)

// applyCommand is the applier Job's entrypoint: the same binary as the
// console, in the one execution context that holds the write credential.
//
// The flag names mirror the peribolos flags this subcommand replaced, so
// the Job spec and the runbooks read the same before and after.
func applyCommand() *cli.Command {
	return &cli.Command{
		Name:  "apply",
		Usage: "reconcile one organization's membership from a rendered document (Job entrypoint)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config-path", Usage: "rendered membership document", Required: true},
			&cli.StringFlag{Name: "org", Usage: "organization to reconcile", Required: true},
			&cli.StringFlag{Name: "mode", Usage: "what the document claims to be: full or removals-only", Required: true},
			&cli.Int64Flag{Name: "github-app-id", Usage: "applier App id", Required: true},
			&cli.Int64Flag{Name: "github-app-installation-id", Usage: "applier App installation id", Required: true},
			&cli.StringFlag{Name: "github-app-private-key-path", Usage: "applier App private key", Required: true},
			&cli.IntFlag{Name: "min-admins", Usage: "refuse a document naming fewer owners", Value: 2},
			&cli.Float64Flag{Name: "maximum-removal-delta", Usage: "refuse removing more than this fraction of members", Value: 0.25},
			&cli.BoolFlag{Name: "confirm", Usage: "actually apply; without it the run is a preview"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			mode := peribolos.Mode(cmd.String("mode"))
			if mode != peribolos.ModeFull && mode != peribolos.ModeRemovalsOnly {
				return fmt.Errorf("unknown mode %q", cmd.String("mode"))
			}

			return reconciler.Run(ctx, reconciler.RunInputs{
				ConfigPath:     cmd.String("config-path"),
				Org:            cmd.String("org"),
				PrivateKeyPath: cmd.String("github-app-private-key-path"),
				AppID:          cmd.Int64("github-app-id"),
				InstallationID: cmd.Int64("github-app-installation-id"),
				Confirm:        cmd.Bool("confirm"),
				Options: reconciler.Options{
					Mode:               mode,
					MinAdmins:          cmd.Int("min-admins"),
					MaxRemovalFraction: cmd.Float64("maximum-removal-delta"),
				},
			}, os.Stdout)
		},
	}
}
