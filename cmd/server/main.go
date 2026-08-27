// Command github-roster runs the roster console.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/truvity/github-roster/pkg/app"
	"github.com/truvity/github-roster/pkg/version"
)

// Injected at release time by goreleaser's ldflags.
var (
	Version = "dev"
	Commit  = ""
)

func main() {
	// os.Exit skips deferred calls, so the whole body lives in run() and
	// main does nothing but translate an error into an exit code.
	os.Exit(run())
}

func run() int {
	info := version.Info{Version: Version, Commit: Commit}

	// SIGTERM is what Kubernetes sends; SIGINT is what a laptop sends.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cmd := &cli.Command{
		Name:    "github-roster",
		Usage:   "GitHub membership console for organizations without Enterprise",
		Version: info.String(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Usage:   "path to the configuration document",
				Sources: cli.EnvVars(app.EnvConfigFile),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return app.Run(ctx, info, cmd.String("config"))
		},
		Commands: []*cli.Command{
			{
				Name:  "broker",
				Usage: "run the applier broker: the write-credential holder behind an intent-only API",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "config",
						Usage:   "path to the configuration document",
						Sources: cli.EnvVars(app.EnvConfigFile),
					},
					&cli.StringFlag{
						Name:  "listen",
						Usage: "address to serve the broker API on",
						Value: ":8080",
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return app.RunBroker(ctx, info, cmd.String("config"), cmd.String("listen"))
				},
			},
			{
				Name:  "version",
				Usage: "print the build stamp and exit",
				Action: func(context.Context, *cli.Command) error {
					fmt.Println(info.String())

					return nil
				},
			},
		},
	}

	if err := cmd.Run(ctx, os.Args); err != nil {
		slog.Error("fatal", slog.Any("error", err))

		return 1
	}

	return 0
}
