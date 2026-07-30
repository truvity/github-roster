// Package selftest verifies that a DEPLOYMENT is correctly wired.
//
// It answers a different question from the test suites. Unit and
// integration tests prove the code; this proves the installation — the IAM
// grants, the KMS key access, the RBAC, the mirrored credentials, the
// reachability — using the deployment's own identity and configuration.
// The things that only a real deployment can get wrong.
//
// Every check is read-only. That is a hard property, not a preference: the
// selftest runs as a `helm test` hook and an ArgoCD PostSync hook on every
// environment INCLUDING production, so it must be incapable of changing
// anything. The mutating counterpart is the acceptance suite, which test
// values enable and production values never do.
package selftest

import (
	"context"
	"fmt"
	"strconv"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/secrets"
)

// Result is one check's outcome.
type Result struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Skipped marks a check that could not run here (e.g. no cluster);
	// skipped is not failed, and says why.
	Skipped bool `json:"skipped,omitempty"`
}

// Report is a whole run.
type Report struct {
	Results []Result `json:"results"`
}

// Failed reports whether any check failed (skips do not count).
func (r *Report) Failed() bool {
	for _, result := range r.Results {
		if !result.OK && !result.Skipped {
			return true
		}
	}

	return false
}

func (r *Report) add(check string, err error, okDetail string) {
	if err != nil {
		r.Results = append(r.Results, Result{Check: check, OK: false, Detail: err.Error()})

		return
	}

	r.Results = append(r.Results, Result{Check: check, OK: true, Detail: okDetail})
}

func (r *Report) skip(check, why string) {
	r.Results = append(r.Results, Result{Check: check, OK: true, Skipped: true, Detail: why})
}

const perCheckTimeout = 30 * time.Second

// Run executes every check against the given configuration.
func Run(ctx context.Context, cfg *config.Config) *Report {
	report := &Report{}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		report.add("aws-config", err, "")

		return report
	}

	ssmClient := ssm.NewFromConfig(awsCfg)
	reader := secrets.NewReader(ssmClient)

	checkMapping(ctx, report, ssmClient, cfg)
	checkAudit(ctx, report, s3.NewFromConfig(awsCfg), cfg)

	for i := range cfg.Orgs {
		org := &cfg.Orgs[i]
		checkConsoleApp(ctx, report, reader, org)
		checkApplierIdentifiers(ctx, report, reader, org)
	}

	checkRBAC(ctx, report)

	return report
}

// checkMapping proves the mapping prefix is readable — which exercises the
// IAM grant AND the KMS decrypt on the SecureStrings in one call.
func checkMapping(ctx context.Context, report *Report, client *ssm.Client, cfg *config.Config) {
	ctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	defer cancel()

	store := mapping.NewSSM(client, cfg.Mapping.SSMPrefix)

	entries, err := store.List(ctx)
	report.add("mapping-readable", err,
		fmt.Sprintf("%d entries under %s (IAM and KMS decrypt both work)", len(entries), cfg.Mapping.SSMPrefix))
}

// checkAudit proves the audit sink is READABLE. Deliberately not writable:
// proving the write path would mean writing, and the selftest runs on
// production on every sync. The write path is proven by the acceptance
// suite on test installs, and by the first real run everywhere else — a
// missing write grant surfaces there as AUDIT RECORD LOST, loudly.
func checkAudit(ctx context.Context, report *Report, client *s3.Client, cfg *config.Config) {
	ctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	defer cancel()

	sink, err := audit.NewS3(client, cfg.Audit.Bucket, cfg.Audit.Prefix, cfg.Audit.PrefixPerOrg)
	if err != nil {
		report.add("audit-readable", err, "")

		return
	}

	records, err := sink.List(ctx, "", 1)
	report.add("audit-readable", err,
		fmt.Sprintf("bucket %s reachable, %d record(s) visible (read path only; writes are proven by real runs)",
			cfg.Audit.Bucket, len(records)))
}

// checkConsoleApp authenticates as the console App and reads the org —
// proving the mirrored credentials parse, the installation is valid, and
// the read permissions hold.
func checkConsoleApp(ctx context.Context, report *Report, reader *secrets.Reader, org *config.Org) {
	ctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	defer cancel()

	check := "console-app-" + org.Name

	values, err := reader.ReadAll(ctx, org.ConsoleAppSSM,
		"github-app-id", "github-installation-id", "github-private-key")
	if err != nil {
		report.add(check, err, "")

		return
	}

	appID, err := strconv.ParseInt(values["github-app-id"], 10, 64)
	if err != nil {
		report.add(check, fmt.Errorf("app id is not a number: %w", err), "")

		return
	}

	installationID, err := strconv.ParseInt(values["github-installation-id"], 10, 64)
	if err != nil {
		report.add(check, fmt.Errorf("installation id is not a number: %w", err), "")

		return
	}

	source, err := githubapp.NewTokenSource(githubapp.Credentials{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     []byte(values["github-private-key"]),
	})
	if err != nil {
		report.add(check, err, "")

		return
	}

	orgReader, err := orgstate.NewReader(source, org.Name, "")
	if err != nil {
		report.add(check, err, "")

		return
	}

	members, err := orgReader.Members(ctx)
	report.add(check, err, fmt.Sprintf("authenticated; %d members visible", len(members)))
}

// checkApplierIdentifiers proves the applier App's identifiers are
// mirrored. The private key is deliberately NOT read — this process never
// opens it, and the selftest must not be the exception to that rule.
func checkApplierIdentifiers(ctx context.Context, report *Report, reader *secrets.Reader, org *config.Org) {
	ctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	defer cancel()

	check := "applier-identifiers-" + org.Name

	values, err := reader.ReadAll(ctx, org.ApplierAppSSM, "github-app-id", "github-installation-id")
	report.add(check, err,
		fmt.Sprintf("app %s installation %s (the key stays sealed; only Jobs mount it)",
			values["github-app-id"], values["github-installation-id"]))
}

// checkRBAC asks the API server whether this ServiceAccount may do exactly
// what the reconciler runner does — via SelfSubjectAccessReview, which
// answers without creating anything and without extra permissions.
//
// The verb list mirrors the chart's Role and pkg/applier's calls; the unit
// suite keeps those two in step, and this check proves the LIVE binding.
func checkRBAC(ctx context.Context, report *Report) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		report.skip("rbac", "not running in a cluster")

		return
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		report.add("rbac", err, "")

		return
	}

	checks := []struct {
		group, resource, subresource, verb string
	}{
		{"batch", "jobs", "", "create"},
		{"batch", "jobs", "", "get"},
		{"", "configmaps", "", "create"},
		{"", "pods", "", "list"},
		{"", "pods", "log", "get"},
		{"coordination.k8s.io", "leases", "", "create"},
		{"coordination.k8s.io", "leases", "", "get"},
		{"coordination.k8s.io", "leases", "", "update"},
	}

	for _, c := range checks {
		ctx, cancel := context.WithTimeout(ctx, perCheckTimeout)

		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group:       c.group,
					Resource:    c.resource,
					Subresource: c.subresource,
					Verb:        c.verb,
				},
			},
		}

		name := fmt.Sprintf("rbac-%s-%s", c.verb, c.resource)
		if c.subresource != "" {
			name += "-" + c.subresource
		}

		answer, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})

		cancel()

		switch {
		case err != nil:
			report.add(name, err, "")
		case !answer.Status.Allowed:
			report.add(name, fmt.Errorf("denied: %s", answer.Status.Reason), "")
		default:
			report.add(name, nil, "allowed")
		}
	}
}
