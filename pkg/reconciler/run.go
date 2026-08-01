package reconciler

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
)

// RunInputs are everything the apply subcommand collects from its flags.
type RunInputs struct {
	// ConfigPath is the rendered document, mounted by the Job.
	ConfigPath string
	// Org is the organization to reconcile. Named explicitly rather than
	// inferred from the document, so a document and a Job that disagree
	// fail loudly instead of applying to whatever the file happens to say.
	Org string
	// PrivateKeyPath is the applier App's key, mounted by the Job. This is
	// the only process in the service that ever reads it.
	PrivateKeyPath string
	// AppID and InstallationID identify the applier App.
	AppID          int64
	InstallationID int64
	// Confirm turns the preview into a real run.
	Confirm bool

	Options Options
}

// Run is the applier Job's whole life: read the document, read the live
// organization, plan, report, and — only with Confirm — execute.
//
// Everything is written to out, which in the Job is stdout: the pod log is
// the run report, and the audit record stores it verbatim.
func Run(ctx context.Context, in RunInputs, out io.Writer) error {
	doc, err := readDocument(in.ConfigPath)
	if err != nil {
		return err
	}

	source, err := tokenSource(in)
	if err != nil {
		return err
	}

	reader, err := orgstate.NewReader(source, in.Org, "")
	if err != nil {
		return err
	}

	state, err := reader.Read(ctx)
	if err != nil {
		return fmt.Errorf("read live state of %q: %w", in.Org, err)
	}

	plan, err := BuildPlan(doc, in.Org, state, in.Options)
	if err != nil {
		return err
	}

	// The report goes to the pod log; a write error there is not a reason
	// to fail a membership run.
	emit := func(format string, args ...any) {
		_, _ = fmt.Fprintf(out, format, args...)
	}

	emit("mode: %s, confirm: %v\n", in.Options.Mode, in.Confirm)
	emit("%s", Report(plan, false))

	if !in.Confirm {
		emit("preview only: nothing was changed\n")

		return nil
	}

	if err := Execute(ctx, NewGitHubWriter(source), plan, func(line string) {
		emit("  %s\n", line)
	}); err != nil {
		return err
	}

	emit("applied %d change(s) to %s\n", len(plan.Actions), plan.Org)

	return nil
}

func readDocument(path string) (*peribolos.Document, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the Job's own mounted ConfigMap
	if err != nil {
		return nil, fmt.Errorf("read document: %w", err)
	}

	doc := &peribolos.Document{}
	if err := yaml.Unmarshal(raw, doc); err != nil {
		return nil, fmt.Errorf("parse document: %w", err)
	}

	if len(doc.Orgs) == 0 {
		return nil, fmt.Errorf("document describes no organizations")
	}

	return doc, nil
}

func tokenSource(in RunInputs) (*githubapp.TokenSource, error) {
	key, err := os.ReadFile(in.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	source, err := githubapp.NewTokenSource(githubapp.Credentials{
		AppID:          in.AppID,
		InstallationID: in.InstallationID,
		PrivateKey:     key,
	})
	if err != nil {
		return nil, fmt.Errorf("applier app credentials: %w", err)
	}

	return source, nil
}
