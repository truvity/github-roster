package runlock

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestMemory(t *testing.T) {
	t.Parallel()

	var m Memory

	release, err := m.TryAcquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.TryAcquire(context.Background(), "b"); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire: want ErrHeld, got %v", err)
	}

	release()

	release2, err := m.TryAcquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}

	release2()
}

func TestLeaseAcquireAndContend(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	lock := NewLease(slog.Default(), client, "test-ns", "run-lock")

	release, err := lock.TryAcquire(context.Background(), "pod-a")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := lock.TryAcquire(context.Background(), "pod-b"); !errors.Is(err, ErrHeld) {
		t.Fatalf("contended acquire: want ErrHeld, got %v", err)
	}

	release()

	release2, err := lock.TryAcquire(context.Background(), "pod-b")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}

	release2()
}

func TestLeaseTakeoverAfterAbandonment(t *testing.T) {
	t.Parallel()

	stale := metav1.NewMicroTime(time.Now().Add(-10 * time.Minute))

	client := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "run-lock", Namespace: "test-ns"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("crashed-pod"),
			LeaseDurationSeconds: ptr.To(int32(120)),
			RenewTime:            ptr.To(stale),
		},
	})

	lock := NewLease(slog.Default(), client, "test-ns", "run-lock")

	release, err := lock.TryAcquire(context.Background(), "pod-b")
	if err != nil {
		t.Fatalf("takeover of abandoned lease: %v", err)
	}

	release()

	lease, err := client.CoordinationV1().Leases("test-ns").Get(context.Background(), "run-lock", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "" {
		t.Fatalf("release should clear the holder, got %v", lease.Spec.HolderIdentity)
	}
}

func TestLeaseHeldByLiveHolder(t *testing.T) {
	t.Parallel()

	fresh := metav1.NewMicroTime(time.Now())

	client := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "run-lock", Namespace: "test-ns"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("live-pod"),
			LeaseDurationSeconds: ptr.To(int32(120)),
			RenewTime:            ptr.To(fresh),
		},
	})

	lock := NewLease(slog.Default(), client, "test-ns", "run-lock")

	if _, err := lock.TryAcquire(context.Background(), "pod-b"); !errors.Is(err, ErrHeld) {
		t.Fatalf("want ErrHeld against a live holder, got %v", err)
	}
}
