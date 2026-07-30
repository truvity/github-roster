// Package runlock serializes removals sweeps across every replica of the
// service.
//
// The in-process mutex this replaces was correct for exactly one replica;
// running two replicas (node-churn resilience, PDB) needs the same
// "one sweep at a time, losers are told rather than queued" contract to
// hold across pods. A Kubernetes Lease is the natural primitive: the lock
// is scoped to a RUN, not to a standing leader — no leader election
// machinery, no leadership to lose between runs.
package runlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// ErrHeld is returned when another holder has the lock. Semantics mirror
// the old TryLock: the caller reports "a run is already in progress".
var ErrHeld = errors.New("the run lock is held by another replica")

// Lock serializes runs. TryAcquire never blocks: it returns a release
// function, or ErrHeld.
type Lock interface {
	TryAcquire(ctx context.Context, holder string) (release func(), err error)
}

// Memory is the single-process implementation — local installs, tests,
// and the fallback when no cluster is available. Zero value is ready.
type Memory struct {
	mu sync.Mutex
}

// TryAcquire implements Lock.
func (m *Memory) TryAcquire(_ context.Context, _ string) (func(), error) {
	if !m.mu.TryLock() {
		return nil, ErrHeld
	}

	return m.mu.Unlock, nil
}

// leaseTTL is how long a holder may go without renewing before the lock
// is considered abandoned (crashed pod) and up for takeover. Renewals run
// at a third of this, so only a real crash — not a slow run — loses it.
const leaseTTL = 2 * time.Minute

// Lease is the multi-replica implementation: a coordination.k8s.io Lease
// used as a mutex. Acquire = create-or-take-if-abandoned; a renewal
// goroutine keeps it fresh for the duration of the run; release clears
// the holder.
type Lease struct {
	logger *slog.Logger
	client kubernetes.Interface
	ns     string
	name   string
}

// NewLease builds a Lease lock in the given namespace.
func NewLease(logger *slog.Logger, client kubernetes.Interface, ns, name string) *Lease {
	return &Lease{logger: logger, client: client, ns: ns, name: name}
}

// TryAcquire implements Lock. Any create/update conflict means another
// replica won the race — reported as ErrHeld, never retried internally.
func (l *Lease) TryAcquire(ctx context.Context, holder string) (func(), error) {
	leases := l.client.CoordinationV1().Leases(l.ns)
	now := metav1.NewMicroTime(time.Now())

	current, err := leases.Get(ctx, l.name, metav1.GetOptions{})

	switch {
	case apierrors.IsNotFound(err):
		created, createErr := leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: l.name, Namespace: l.ns},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holder),
				LeaseDurationSeconds: ptr.To(int32(leaseTTL.Seconds())),
				AcquireTime:          ptr.To(now),
				RenewTime:            ptr.To(now),
				LeaseTransitions:     ptr.To(int32(1)),
			},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(createErr) {
			return nil, ErrHeld
		}

		if createErr != nil {
			return nil, fmt.Errorf("create run lease: %w", createErr)
		}

		return l.hold(created, holder), nil

	case err != nil:
		return nil, fmt.Errorf("get run lease: %w", err)
	}

	if heldByOther(current, holder, now.Time) {
		return nil, ErrHeld
	}

	transitions := int32(1)
	if current.Spec.LeaseTransitions != nil {
		transitions = *current.Spec.LeaseTransitions + 1
	}

	current.Spec.HolderIdentity = ptr.To(holder)
	current.Spec.LeaseDurationSeconds = ptr.To(int32(leaseTTL.Seconds()))
	current.Spec.AcquireTime = ptr.To(now)
	current.Spec.RenewTime = ptr.To(now)
	current.Spec.LeaseTransitions = ptr.To(transitions)

	updated, err := leases.Update(ctx, current, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return nil, ErrHeld
	}

	if err != nil {
		return nil, fmt.Errorf("take over run lease: %w", err)
	}

	return l.hold(updated, holder), nil
}

// heldByOther reports whether the lease belongs to a live other holder.
// An empty holder or an expired renewal means abandoned — up for grabs.
func heldByOther(lease *coordinationv1.Lease, holder string, now time.Time) bool {
	spec := lease.Spec
	if spec.HolderIdentity == nil || *spec.HolderIdentity == "" || *spec.HolderIdentity == holder {
		return false
	}

	if spec.RenewTime == nil {
		return false
	}

	ttl := leaseTTL
	if spec.LeaseDurationSeconds != nil {
		ttl = time.Duration(*spec.LeaseDurationSeconds) * time.Second
	}

	return now.Before(spec.RenewTime.Add(ttl))
}

// hold starts renewal and returns the release function. Renewal runs on
// its own context: the run's ctx canceling mid-sweep must not silently
// drop the lock while cleanup is still writing.
func (l *Lease) hold(lease *coordinationv1.Lease, holder string) func() {
	renewCtx, stop := context.WithCancel(context.Background())

	go func() {
		ticker := time.NewTicker(leaseTTL / 3)
		defer ticker.Stop()

		current := lease

		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				now := metav1.NewMicroTime(time.Now())
				current.Spec.RenewTime = ptr.To(now)

				updated, err := l.client.CoordinationV1().Leases(l.ns).Update(renewCtx, current, metav1.UpdateOptions{})
				if err != nil {
					if renewCtx.Err() == nil {
						l.logger.Warn("run lease renewal failed", slog.Any("error", err))
					}

					continue
				}

				current = updated
			}
		}
	}()

	return func() {
		stop()

		// Best-effort release: clear the holder so the next replica does
		// not wait out the TTL. A failure here only costs that wait.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		current, err := l.client.CoordinationV1().Leases(l.ns).Get(ctx, l.name, metav1.GetOptions{})
		if err != nil {
			return
		}

		if current.Spec.HolderIdentity == nil || *current.Spec.HolderIdentity != holder {
			return
		}

		current.Spec.HolderIdentity = ptr.To("")

		_, _ = l.client.CoordinationV1().Leases(l.ns).Update(ctx, current, metav1.UpdateOptions{})
	}
}
