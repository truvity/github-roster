package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/gofiber/fiber/v3"
	slogfiber "github.com/samber/slog-fiber"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/broker"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/secrets"
	"github.com/truvity/github-roster/pkg/version"
)

// RunBroker starts the applier broker: the process that holds the write
// credential. It shares the console's configuration document; what
// distinguishes it is WHICH credentials it reads — the applier App's,
// including the private key the console never opens.
func RunBroker(ctx context.Context, info version.Info, configPath, listen string) error {
	if configPath == "" {
		return fmt.Errorf("no configuration document: pass --config or set %s", EnvConfigFile)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	logger.InfoContext(ctx, "starting applier broker",
		slog.String("version", info.String()),
		slog.Int("orgs", len(cfg.Orgs)))

	deps, err := buildBrokerDeps(ctx, logger, cfg)
	if err != nil {
		return err
	}

	app := fiber.New(fiber.Config{
		AppName: "github-roster-broker " + info.String(),
		// The console forwards the operator's bearer token, and a Zitadel
		// JWT with role assertions alone can exceed fasthttp's 4 KiB
		// default — which answers 431 before any handler runs. Same
		// lesson, same number as the console's own listener.
		ReadBufferSize: 64 << 10,
	})
	app.Use(slogfiber.New(logger))

	deps.Routes(app)

	// The unattended half lives here now: the credential holder acts on
	// its own schedule and its own reads.
	go deps.Schedule(ctx)

	// The continuous reconcile loop (0.17): computes full desired state per
	// org every interval and applies it where the org is enabled. Orgs are
	// born disabled (the day-0 gate), so this is compute-and-report until an
	// operator turns one on. See docs/architecture/reconciliation.md.
	go deps.Reconcile(ctx)

	errs := make(chan error, 1)

	go func() {
		errs <- app.Listen(listen, fiber.ListenConfig{DisableStartupMessage: true})
	}()

	logger.InfoContext(ctx, "broker listening", slog.String("addr", listen))

	select {
	case <-ctx.Done():
		return app.Shutdown()
	case err := <-errs:
		return err
	}
}

// buildBrokerDeps reads the broker's credentials and wires its layers.
//
// This is the ONE place in the whole service where the applier App's
// private key is read: the broker is the write boundary now, and the SSM
// prefix it reads from is IAM-scoped to its service account alone.
func buildBrokerDeps(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*broker.Deps, error) {
	authenticator, err := buildAuth(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}

	ssmClient := ssm.NewFromConfig(awsCfg)
	reader := secrets.NewReader(ssmClient)

	sources, err := buildSources(ctx, reader, cfg)
	if err != nil {
		return nil, err
	}

	directories := directory.NewSet(logger, sources...)

	// Refuse to start without an audit sink: a broker that cannot record
	// what it does must not act.
	sink, err := audit.NewS3(s3.NewFromConfig(awsCfg), cfg.Audit.Bucket, cfg.Audit.Prefix, cfg.Audit.PrefixPerOrg)
	if err != nil {
		return nil, err
	}

	orgs := make(map[string]*broker.Org, len(cfg.Orgs))

	for i := range cfg.Orgs {
		org := &cfg.Orgs[i]

		handle, err := buildBrokerOrg(ctx, reader, org)
		if err != nil {
			return nil, err
		}

		orgs[org.Name] = handle
	}

	return &broker.Deps{
		Logger:      logger,
		Config:      cfg,
		Auth:        authenticator,
		Mapping:     mapping.NewSSM(ssmClient, cfg.Mapping.SSMPrefix),
		Directories: directories,
		Orgs:        orgs,
		Audit:       sink,
	}, nil
}

// buildBrokerOrg reads one organization's applier credentials — key
// included — and builds its reader and token source.
func buildBrokerOrg(ctx context.Context, reader *secrets.Reader, org *config.Org) (*broker.Org, error) {
	values, err := reader.ReadAll(ctx, org.ApplierAppSSM, fieldAppID, fieldInstallationID, fieldPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("org %q applier credentials: %w", org.Name, err)
	}

	appID, err := strconv.ParseInt(values[fieldAppID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("org %q: app id %q is not a number: %w", org.Name, values[fieldAppID], err)
	}

	installationID, err := strconv.ParseInt(values[fieldInstallationID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("org %q: installation id is not a number: %w", org.Name, err)
	}

	source, err := githubapp.NewTokenSource(githubapp.Credentials{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     []byte(values[fieldPrivateKey]),
	})
	if err != nil {
		return nil, fmt.Errorf("org %q: %w", org.Name, err)
	}

	stateReader, err := orgstate.NewReader(source, org.Name, "")
	if err != nil {
		return nil, err
	}

	return &broker.Org{Reader: stateReader, Source: source}, nil
}
