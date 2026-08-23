package reconciler_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/truvity/github-roster/pkg/reconciler"
)

func TestIsGuardError(t *testing.T) {
	guard := fmt.Errorf("%w: below minimum owners", reconciler.ErrGuard)
	if !reconciler.IsGuardError(guard) {
		t.Fatal("a wrapped ErrGuard must be a guard error")
	}

	plain := errors.New("read organization: timeout")
	if reconciler.IsGuardError(plain) {
		t.Fatal("an unrelated error must not be a guard error")
	}

	if reconciler.IsGuardError(nil) {
		t.Fatal("nil is not a guard error")
	}
}
