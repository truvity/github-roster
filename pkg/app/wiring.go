package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/secrets"
	"github.com/truvity/github-roster/pkg/server"
)

// Field names in the mirrored credential sets. They match the mirror
// manifest, so a rename there is a compile-time-visible rename here.
const (
	fieldServiceAccountKey = "google-service-account-key-json"
	fieldAdminEmail        = "google-admin-email"

	fieldAppID          = "github-app-id"
	fieldInstallationID = "github-installation-id"
	fieldPrivateKey     = "github-private-key"
)

// readLayers is everything the service reads from.
type readLayers struct {
	Mapping     mapping.Store
	Directories *directory.Set
	// Orgs holds one reader per managed organization, keyed by org name.
	Orgs map[string]server.OrgReader
	// ApplierApps holds each organization's applier App IDENTIFIERS.
	ApplierApps map[string]server.ApplierApp
	// Audit records every run.
	Audit audit.Sink
}

// buildReadLayers constructs the read side from configuration.
//
// Credentials are read here, at startup, rather than lazily on first use: a
// missing parameter or a malformed key should stop the rollout, not surface
// as a broken page an hour later when somebody opens it.
func buildReadLayers(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*readLayers, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}

	ssmClient := ssm.NewFromConfig(awsCfg)
	reader := secrets.NewReader(ssmClient)

	layers := &readLayers{
		Mapping:     mapping.NewSSM(ssmClient, cfg.Mapping.SSMPrefix),
		Orgs:        make(map[string]server.OrgReader, len(cfg.Orgs)),
		ApplierApps: make(map[string]server.ApplierApp, len(cfg.Orgs)),
	}

	sources, err := buildSources(ctx, reader, cfg)
	if err != nil {
		return nil, err
	}

	layers.Directories = directory.NewSet(logger, sources...)

	// The audit sink. Required rather than optional: a deployment that
	// cannot record what it did should not be quietly acting anyway.
	layers.Audit, err = audit.NewS3(s3.NewFromConfig(awsCfg), cfg.Audit.Bucket, cfg.Audit.Prefix, cfg.Audit.PrefixPerOrg)
	if err != nil {
		return nil, err
	}

	for i := range cfg.Orgs {
		org := &cfg.Orgs[i]

		// The CONSOLE credential, deliberately. The applier's credentials
		// are never read by this process — they are mounted into the
		// reconciler Job and nowhere else.
		orgReader, err := buildOrgReader(ctx, reader, org)
		if err != nil {
			return nil, err
		}

		layers.Orgs[org.Name] = orgReader

		// The applier App's IDENTIFIERS only. The private key at this same
		// prefix is deliberately NOT read: it is mounted straight into the
		// reconciler Job from a Secret, and this process never opens it.
		// Reading it here — even to pass it along — would put the write
		// credential in the web tier's memory and undo the boundary the
		// whole design rests on.
		applierIDs, err := reader.ReadAll(ctx, org.ApplierAppSSM, fieldAppID, fieldInstallationID)
		if err != nil {
			return nil, fmt.Errorf("org %q applier identifiers: %w", org.Name, err)
		}

		layers.ApplierApps[org.Name] = server.ApplierApp{
			AppID:          applierIDs[fieldAppID],
			InstallationID: applierIDs[fieldInstallationID],
		}
	}

	return layers, nil
}

func buildSources(ctx context.Context, reader *secrets.Reader, cfg *config.Config) ([]directory.Source, error) {
	sources := make([]directory.Source, 0, len(cfg.Sources))

	for i := range cfg.Sources {
		src := &cfg.Sources[i]

		values, err := reader.ReadAll(ctx, src.SSMPrefix, fieldServiceAccountKey, fieldAdminEmail)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", src.Name, err)
		}

		source, err := directory.NewGoogle(directory.GoogleConfig{
			Name:    src.Name,
			Domains: src.Domains,
			// Only the groups some team maps to AND this source's
			// directory owns — a source asked about a foreign company's
			// group 403s and the whole fetch fails.
			Groups:  cfg.MappedGroupsForDomains(src.Domains),
			KeyJSON: []byte(values[fieldServiceAccountKey]),
			Subject: values[fieldAdminEmail],
		})
		if err != nil {
			return nil, err
		}

		sources = append(sources, source)
	}

	return sources, nil
}

func buildOrgReader(ctx context.Context, reader *secrets.Reader, org *config.Org) (*orgstate.Reader, error) {
	values, err := reader.ReadAll(ctx, org.ConsoleAppSSM, fieldAppID, fieldInstallationID, fieldPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("org %q console credentials: %w", org.Name, err)
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

	return orgstate.NewReader(source, org.Name, "")
}

// buildApplier connects the reconciler runner, or returns nil.
//
// Nil is a legitimate outcome, not a failure: the console runs perfectly
// well outside a cluster for local work and for the integration suite, and
// the sync surface then reports itself unavailable rather than pretending
// it could act. Failing startup instead would make the service impossible
// to run anywhere but its final home.
func buildApplier(logger *slog.Logger, cfg *config.Config) (*applier.Runner, error) {
	if cfg.Reconciler.Image == "" {
		logger.Warn("no reconciler image configured; the sync surface will report itself unavailable")

		return nil, nil
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Warn("not running in a cluster; the sync surface will report itself unavailable",
			slog.Any("error", err))

		return nil, nil
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	namespace := cfg.Reconciler.Namespace
	if namespace == "" {
		namespace = currentNamespace()
	}

	if namespace == "" {
		return nil, fmt.Errorf("reconciler.namespace is empty and this pod's namespace could not be read")
	}

	return applier.NewRunner(client, applier.Options{
		Namespace:      namespace,
		Image:          cfg.Reconciler.Image,
		ServiceAccount: cfg.Reconciler.ServiceAccount,
		Timeout:        cfg.Reconciler.Timeout,
		MinAdmins:      cfg.Reconciler.MinAdmins,
		// One number rather than two that can disagree: the configured
		// shrink threshold drives the reconciler's own guard.
		MaxRemovalFraction: cfg.Schedule.MaxRemovalFraction,
	})
}

// currentNamespace reads the namespace the pod runs in.
func currentNamespace() string {
	const path = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}
