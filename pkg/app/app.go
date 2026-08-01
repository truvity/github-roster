// Package app is the wiring: one function that builds every component from
// the configuration and runs the service. No DI container by design — hand
// wiring is cheap to write, and it is the one place a reader can see the
// whole shape of the process.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/broker"
	"github.com/truvity/github-roster/pkg/config"

	"github.com/truvity/github-roster/pkg/runlock"
	"github.com/truvity/github-roster/pkg/server"
	"github.com/truvity/github-roster/pkg/ui"
	"github.com/truvity/github-roster/pkg/version"
)

// Environment contract.
//
// It is short, and that is the point: the gateway in front of this service
// owns the sign-in, so there is no client secret to hold and no session key
// to manage, share between replicas or rotate.
const (
	// EnvConfigFile names the configuration document.
	EnvConfigFile = "CONFIG_FILE"
	// EnvLogLevel and EnvLogFormat override the document, so a debug session
	// does not need a config change.
	EnvLogLevel  = "LOG_LEVEL"
	EnvLogFormat = "LOG_FORMAT"
)

// Run loads configuration, builds everything and serves until ctx is done.
func Run(ctx context.Context, info version.Info, configPath string) error {
	if configPath == "" {
		return fmt.Errorf("no configuration document: pass --config or set %s", EnvConfigFile)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	logger.InfoContext(ctx, "starting github-roster",
		slog.String("version", info.String()),
		slog.String("config", configPath),
		slog.Int("orgs", len(cfg.Orgs)),
		slog.Int("sources", len(cfg.Sources)))

	deps, err := BuildDeps(ctx, logger, cfg, info)
	if err != nil {
		return err
	}

	return server.Run(ctx, deps)
}

// BuildDeps constructs everything the server needs from a parsed config.
//
// Exported so the integration suite can exercise the REAL startup path —
// credentials read from Parameter Store, App authentication, the directory
// refresh — rather than assembling components by hand and testing a wiring
// that the binary does not use.
func BuildDeps(ctx context.Context, logger *slog.Logger, cfg *config.Config, info version.Info) (*server.Deps, error) {
	authenticator, err := buildAuth(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}

	renderer, err := ui.NewRenderer(info.String())
	if err != nil {
		return nil, err
	}

	layers, err := buildReadLayers(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}

	// One refresh before serving, so a directory that cannot be reached is
	// visible in the logs at rollout rather than discovered by an operator
	// staring at an empty page. It is deliberately not fatal: the console
	// must still come up to SHOW that a source is unhealthy.
	for name, err := range layers.Directories.Refresh(ctx) {
		logger.WarnContext(ctx, "directory source unhealthy at startup",
			slog.String("source", name),
			slog.Any("error", err))
	}

	reconciler, err := buildApplier(logger, cfg)
	if err != nil {
		return nil, err
	}

	// Assigned via a variable so a nil *applier.Runner never becomes a
	// non-nil interface — the typed-nil trap would make every "is the
	// reconciler configured" check silently pass.
	var jobRunner server.JobRunner
	if reconciler != nil {
		jobRunner = reconciler
	}

	// The broker client carries no credentials: every call forwards the
	// operator's own bearer token, so the broker authorizes the human.
	var brokerClient *broker.Client
	if cfg.Broker.URL != "" {
		brokerClient = broker.NewClient(cfg.Broker.URL)
	} else {
		logger.Warn("no broker configured; the sync surface will report itself unavailable")
	}

	return &server.Deps{
		Logger:      logger,
		Config:      cfg,
		RunLock:     buildRunLock(logger),
		Auth:        authenticator,
		Renderer:    renderer,
		Version:     info,
		Mapping:     layers.Mapping,
		Directories: layers.Directories,
		Orgs:        layers.Orgs,
		Applier:     jobRunner,
		Broker:      brokerClient,
		Audit:       layers.Audit,
		ApplierApps: layers.ApplierApps,
	}, nil
}

func buildAuth(ctx context.Context, logger *slog.Logger, cfg *config.Config) (auth.Authenticator, error) {
	if cfg.OIDC.Disabled {
		return auth.NewDisabled(logger), nil
	}

	return auth.NewVerifier(ctx, logger, cfg.OIDC)
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := cfg.LogLevel
	if v := os.Getenv(EnvLogLevel); v != "" {
		level = v
	}

	format := cfg.LogFormat
	if v := os.Getenv(EnvLogFormat); v != "" {
		format = v
	}

	var lvl slog.Level

	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// buildRunLock picks the sweep-serialization scope: a Kubernetes Lease
// when running in a cluster (the lock must span replicas), the in-process
// fallback otherwise. Never fatal — a local install without a cluster is
// a supported shape.
func buildRunLock(logger *slog.Logger) runlock.Lock {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Info("run lock: in-process (not running in a cluster)")

		return nil
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Warn("run lock: in-process (cluster client failed)", slog.Any("error", err))

		return nil
	}

	ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		logger.Warn("run lock: in-process (namespace unknown)", slog.Any("error", err))

		return nil
	}

	logger.Info("run lock: kubernetes lease", slog.String("namespace", strings.TrimSpace(string(ns))))

	return runlock.NewLease(logger, client, strings.TrimSpace(string(ns)), "github-roster-run")
}
