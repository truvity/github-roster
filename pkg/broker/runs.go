package broker

import (
	"bufio"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/reconciler"
)

// runTTL is how long a finished run's transcript stays readable. Long
// enough to open the page after coffee; the audit record is the durable
// account.
const runTTL = time.Hour

// RunStatus is where an asynchronous apply is in its life.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
)

// run is one asynchronous apply: its transcript so far and everyone
// watching it live.
type run struct {
	ID     string
	Org    string
	Hash   string
	status RunStatus

	mu    sync.Mutex
	lines []string
	// subscribers receive each new line; closed when the run ends.
	subscribers map[chan string]bool
	finishedAt  time.Time
}

// append records a line and fans it out to live watchers.
func (r *run) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines = append(r.lines, line)

	for ch := range r.subscribers {
		select {
		case ch <- line:
		default: // a stalled watcher never stalls the apply
		}
	}
}

// finish marks the run done and releases every watcher.
func (r *run) finish(status RunStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.status = status
	r.finishedAt = time.Now()

	for ch := range r.subscribers {
		close(ch)
		delete(r.subscribers, ch)
	}
}

// snapshot returns the transcript so far, the status, and a channel of
// future lines (nil when the run is over).
func (r *run) snapshot() (lines []string, status RunStatus, tail chan string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	lines = append(lines, r.lines...)

	if r.status != RunRunning {
		return lines, r.status, nil
	}

	tail = make(chan string, 64)
	r.subscribers[tail] = true

	return lines, r.status, tail
}

func (r *run) unsubscribe(ch chan string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.subscribers[ch] {
		delete(r.subscribers, ch)
		close(ch)
	}
}

// runRegistry keeps recent runs in memory — like plans, derived data on
// a single replica.
type runRegistry struct {
	mu   sync.Mutex
	runs map[string]*run
}

func newRunRegistry() *runRegistry {
	return &runRegistry{runs: map[string]*run{}}
}

func (g *runRegistry) create(org, hash string) *run {
	g.mu.Lock()
	defer g.mu.Unlock()

	for id, old := range g.runs {
		old.mu.Lock()
		expired := old.status != RunRunning && time.Since(old.finishedAt) > runTTL
		old.mu.Unlock()

		if expired {
			delete(g.runs, id)
		}
	}

	r := &run{
		ID:          fmt.Sprintf("run-%s", time.Now().UTC().Format("20060102t150405.000")),
		Org:         org,
		Hash:        hash,
		status:      RunRunning,
		subscribers: map[chan string]bool{},
	}
	g.runs[r.ID] = r

	return r
}

func (g *runRegistry) get(id string) (*run, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	r, ok := g.runs[id]

	return r, ok
}

// handleApplyAsync starts an apply and answers immediately with the run
// id; the transcript streams from /runs/{id}/stream. Same guards, same
// hash contract, same audit record as the synchronous path.
func (d *Deps) handleApplyAsync(c fiber.Ctx) error {
	org, name, err := d.org(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	approved := c.Params("hash")
	if _, ok := d.plans.Get(approved); !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such plan (it may have expired); compute a new one"})
	}

	identity, _ := auth.From(c)
	actor := auditActor(identity)

	r := d.runs.create(name, approved)

	// The request's context dies with the response; the apply must not.
	// Detaching from the request context is the point of the async path.
	go d.runApply(context.Background(), r, name, org, approved, actor) //nolint:contextcheck,gosec // G118: deliberate detachment

	return c.JSON(fiber.Map{"runId": r.ID, "org": name, "hash": approved})
}

// runApply is the asynchronous body: serialize, apply, record, narrate.
func (d *Deps) runApply(ctx context.Context, r *run, name string, org *Org, approved string, actor audit.Actor) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	r.append("waiting for the run lock…")
	d.applyMu.Lock()
	defer d.applyMu.Unlock()

	r.append("recomputing the plan against fresh state…")

	_, applied, _, err := d.applyStreaming(ctx, r, name, org, approved, actor)
	if err != nil {
		r.append("FAILED: " + err.Error())
		r.finish(RunFailed)

		return
	}

	r.append(fmt.Sprintf("done: %d change(s) applied", applied))
	r.finish(RunSucceeded)
}

// applyStreaming mirrors apply() but narrates into the run.
func (d *Deps) applyStreaming(ctx context.Context, r *run, name string, org *Org, approved string, actor audit.Actor,
) (fresh *stored, applied int, report string, err error) {
	fresh, err = d.compute(ctx, name, org)
	if err != nil {
		return nil, 0, "", err
	}

	if Hash(name, fresh.Mode, fresh.Plan) != approved {
		return fresh, 0, "", fmt.Errorf("the organization changed since this plan was reviewed (fresh hash %s); compute and review again",
			Hash(name, fresh.Mode, fresh.Plan))
	}

	r.append(fmt.Sprintf("hash verified; executing %d change(s)…", len(fresh.Plan.Actions)))

	writer, err := reconciler.NewGitHubWriter(org.Source)
	if err != nil {
		return fresh, 0, "", err
	}

	var lines []string

	execErr := reconciler.Execute(ctx, writer, fresh.Plan, func(line string) {
		lines = append(lines, line)
		r.append(line)
	})

	report = reconciler.Report(fresh.Plan, execErr == nil)

	auditErr := d.record(ctx, audit.TriggerOperator, actor, fresh, report, execErr)
	if auditErr != "" {
		r.append("AUDIT: " + auditErr)
	}

	if execErr != nil {
		return fresh, len(lines), report, execErr
	}

	return fresh, len(fresh.Plan.Actions), report, nil
}

// handleRunStream replays a run's transcript and tails it live, as
// Server-Sent Events: one-directional, proxy-friendly, reconnecting for
// free — the right transport for narration.
func (d *Deps) handleRunStream(c fiber.Ctx) error {
	r, ok := d.runs.get(c.Params("id"))
	if !ok || r.Org != c.Params("org") {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such run (it may have expired)"})
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set("X-Accel-Buffering", "no")

	lines, status, tail := r.snapshot()

	c.Response().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		emit := func(event, data string) bool {
			_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)

			return err == nil && w.Flush() == nil
		}

		for _, line := range lines {
			if !emit("line", line) {
				return
			}
		}

		if tail == nil {
			emit("done", string(status))

			return
		}

		defer r.unsubscribe(tail)

		for line := range tail {
			if !emit("line", line) {
				return
			}
		}

		_, finalStatus, _ := r.snapshot()
		emit("done", string(finalStatus))
	}))

	return nil
}
