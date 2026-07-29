package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/secrets"
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
	Orgs map[string]*orgstate.Reader
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
		Mapping: mapping.NewSSM(ssmClient, cfg.Mapping.SSMPrefix),
		Orgs:    make(map[string]*orgstate.Reader, len(cfg.Orgs)),
	}

	sources, err := buildSources(ctx, reader, cfg)
	if err != nil {
		return nil, err
	}

	layers.Directories = directory.NewSet(logger, sources...)

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
			// Only the groups some team actually maps to. Listing every
			// group in a Workspace would read membership the service has
			// no use for.
			Groups:  cfg.MappedGroups(),
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
