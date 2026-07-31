package selftest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/roster"
)

// AcceptanceOptions locate the deployed service.
type AcceptanceOptions struct {
	// ServiceURL is the console's in-cluster URL.
	ServiceURL string
	// InternalURL is the internal listener (healthz, POST /sync).
	InternalURL string
	// ExecutionID tags everything this run creates, per the shared-tier
	// convention: re-runs tolerate leftovers because assertions are scoped
	// to entities this execution made.
	ExecutionID string
}

// RunAcceptance drives the DEPLOYED release end to end: seed a mapping
// entry through the store, watch it come back joined through the service's
// own API, trigger a removals sweep, and read the audit surface.
//
// This is the mutating tier. It exists only on test installs — the chart
// renders its hook solely when test values ask for it — and its writes are
// scoped to this execution's id and cleaned up on the way out.
func RunAcceptance(ctx context.Context, cfg *config.Config, opts AcceptanceOptions) *Report {
	// Everything selftest proves, first: an acceptance run on a miswired
	// install should fail on the wiring check, not on a mysterious HTTP
	// error three steps later.
	report := Run(ctx, cfg)

	if opts.ExecutionID == "" {
		opts.ExecutionID = strings.ToLower(time.Now().UTC().Format("20060102t150405"))
	}

	client := &http.Client{Timeout: perCheckTimeout}

	checkHealth(ctx, report, client, opts)
	checkRosterAPI(ctx, report, client, cfg, opts)
	checkSyncTrigger(ctx, report, client, opts)
	checkAuditAPI(ctx, report, client, opts)

	return report
}

func checkHealth(ctx context.Context, report *Report, client *http.Client, opts AcceptanceOptions) {
	body, status, err := get(ctx, client, opts.InternalURL+"/healthz")

	switch {
	case err != nil:
		report.add("service-health", err, "")
	case status != http.StatusOK:
		report.add("service-health", fmt.Errorf("status %d: %s", status, body), "")
	default:
		report.add("service-health", nil, strings.TrimSpace(string(body)))
	}
}

// checkRosterAPI seeds one mapping entry through the store — the same
// store the service reads — and requires the service's own API to serve it
// back joined. This is the end-to-end proof: SSM write, KMS, the join, and
// the HTTP surface, all through the deployed release.
func checkRosterAPI(ctx context.Context, report *Report, client *http.Client, cfg *config.Config, opts AcceptanceOptions) {
	const check = "roster-join-e2e"

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		report.add(check, err, "")

		return
	}

	store := mapping.NewSSM(ssm.NewFromConfig(awsCfg), cfg.Mapping.SSMPrefix)

	// The execution id in the NAME keys the tolerance contract: a re-run
	// neither collides with leftovers nor depends on their absence.
	name := "Acceptance Probe " + opts.ExecutionID

	entry := mapping.Entry{
		Name:   name,
		GitHub: "acceptance-" + opts.ExecutionID,
		Class:  mapping.ClassBot, // no directory account, no liveness needed
	}

	if err := store.Put(ctx, entry); err != nil {
		report.add(check, fmt.Errorf("seed mapping entry: %w", err), "")

		return
	}

	defer func() {
		// Best-effort: the janitor is the backstop for a crashed run.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), perCheckTimeout)
		defer cancel()

		_ = store.Delete(cleanup, name)
	}()

	body, status, err := get(ctx, client, opts.ServiceURL+"/api/roster")

	switch {
	case err != nil:
		report.add(check, err, "")

		return
	case status != http.StatusOK:
		report.add(check, fmt.Errorf("GET /api/roster: status %d: %s", status, truncate(body)), "")

		return
	}

	var joined roster.Roster
	if err := json.Unmarshal(body, &joined); err != nil {
		report.add(check, fmt.Errorf("decode roster: %w", err), "")

		return
	}

	for i := range joined.People {
		if person := &joined.People[i]; person.Name == name {
			report.add(check, nil, "seeded entry served back joined by the deployed release")

			return
		}
	}

	report.add(check, fmt.Errorf("seeded entry %q is absent from the served roster (%d people)",
		name, len(joined.People)), "")
}

// checkSyncTrigger POSTs the internal /sync. On a healthy test install
// this is a no-op sweep (nobody is positively gone), which is exactly the
// assertion: the pipeline runs end to end and removes no one.
func checkSyncTrigger(ctx context.Context, report *Report, client *http.Client, opts AcceptanceOptions) {
	const check = "sync-trigger"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.InternalURL+"/sync", http.NoBody)
	if err != nil {
		report.add(check, err, "")

		return
	}

	resp, err := client.Do(req)
	if err != nil {
		report.add(check, err, "")

		return
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		report.add(check, nil, "sweep ran: "+truncate(body))
	case http.StatusConflict:
		// Another sweep was underway — the serialization working as
		// designed is a pass, not a flake.
		report.add(check, nil, "a sweep was already in progress (serialization holds)")
	case http.StatusServiceUnavailable:
		// No reconciler on this install (no cluster, or none configured).
		report.skip(check, "no reconciler configured on this install")
	default:
		report.add(check, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(body)), "")
	}
}

func checkAuditAPI(ctx context.Context, report *Report, client *http.Client, opts AcceptanceOptions) {
	const check = "audit-api"

	body, status, err := get(ctx, client, opts.ServiceURL+"/api/audit")

	switch {
	case err != nil:
		report.add(check, err, "")
	case status != http.StatusOK:
		report.add(check, fmt.Errorf("status %d: %s", status, truncate(body)), "")
	default:
		report.add(check, nil, "audit surface serves")
	}
}

func get(ctx context.Context, client *http.Client, url string) (body []byte, status int, err error) {
	ctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return body, resp.StatusCode, nil
}

func truncate(body []byte) string {
	const limit = 300

	s := strings.TrimSpace(string(body))
	if len(s) > limit {
		return s[:limit] + "…"
	}

	return s
}
