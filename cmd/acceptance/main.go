// Command roster-acceptance verifies a DEPLOYED github-roster release.
//
// A separate binary and a separate image from the service, on purpose: the
// production image carries no test code and no test dependencies, so its
// surface and SBOM stay exactly what the service needs. The chart runs this
// image as its `helm test` hooks.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/selftest"
	"github.com/truvity/github-roster/pkg/version"
)

// Injected at release time by goreleaser's ldflags.
var (
	Version = "dev"
	Commit  = ""
)

func main() {
	os.Exit(run())
}

func run() int {
	info := version.Info{Version: Version, Commit: Commit}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configFlag := &cli.StringFlag{
		Name:    "config",
		Usage:   "path to the service's configuration document",
		Sources: cli.EnvVars("CONFIG_FILE"),
	}

	cmd := &cli.Command{
		Name:    "roster-acceptance",
		Usage:   "verify a deployed github-roster release",
		Version: info.String(),
		Commands: []*cli.Command{
			{
				Name:  "selftest",
				Usage: "read-only wiring checks — safe on every environment, production included",
				Flags: []cli.Flag{configFlag},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg, err := loadConfig(cmd)
					if err != nil {
						return err
					}

					return emit(selftest.Run(ctx, cfg))
				},
			},
			{
				Name: "acceptance",
				Usage: "mutating end-to-end checks against the deployed release — " +
					"test installs only; every write is execution-scoped and cleaned up",
				Flags: []cli.Flag{
					configFlag,
					&cli.StringFlag{
						Name:    "service-url",
						Usage:   "the console's in-cluster URL",
						Sources: cli.EnvVars("SERVICE_URL"),
					},
					&cli.StringFlag{
						Name:    "internal-url",
						Usage:   "the internal listener's URL (healthz, sync)",
						Sources: cli.EnvVars("INTERNAL_URL"),
					},
					&cli.StringFlag{
						Name:    "execution-id",
						Usage:   "tags everything this run creates; defaults to a timestamp",
						Sources: cli.EnvVars("EXECUTION_ID"),
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg, err := loadConfig(cmd)
					if err != nil {
						return err
					}

					if cmd.String("service-url") == "" || cmd.String("internal-url") == "" {
						return fmt.Errorf("--service-url and --internal-url are required")
					}

					return emit(selftest.RunAcceptance(ctx, cfg, selftest.AcceptanceOptions{
						ServiceURL:  cmd.String("service-url"),
						InternalURL: cmd.String("internal-url"),
						ExecutionID: cmd.String("execution-id"),
					}))
				},
			},
		},
	}

	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)

		return 1
	}

	return 0
}

func loadConfig(cmd *cli.Command) (*config.Config, error) {
	path := cmd.String("config")
	if path == "" {
		return nil, fmt.Errorf("no configuration document: pass --config or set CONFIG_FILE")
	}

	return config.Load(path)
}

// emit prints every result, then fails the process if any check failed —
// which is what turns the report into a helm test verdict.
func emit(report *selftest.Report) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(encoded))

	if report.Failed() {
		return fmt.Errorf("one or more checks failed")
	}

	return nil
}
