package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/reconciler"
)

// planTTL is how long a computed plan may be applied. An operator who
// walks away for longer re-reviews: the world their plan described has
// had an hour to drift.
const planTTL = time.Hour

// Hash names a plan by its content: organization, mode, and the exact
// ordered action list. Two plans that would do the same thing have the
// same hash; anything else differs. The apply endpoint's whole contract
// is built on this.
func Hash(org string, mode peribolos.Mode, plan *reconciler.Plan) string {
	// Actions are already deterministically ordered by BuildPlan; the
	// struct is marshaled field-by-field, so this is canonical.
	payload, _ := json.Marshal(struct {
		Org     string
		Mode    peribolos.Mode
		Actions []reconciler.Action
	}{org, mode, plan.Actions})

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:8])
}

// stored is one computed plan, kept server-side. The console refers to it
// only by hash — plan content never round-trips through the client.
type stored struct {
	Org       string
	Mode      peribolos.Mode
	Plan      *reconciler.Plan
	Result    *peribolos.Result
	CreatedAt time.Time
}

// planStore keeps recent plans in memory. In memory on purpose: plans are
// derived data, small, and single-replica — persistence would only let a
// stale plan outlive the state that justified it.
type planStore struct {
	mu    sync.Mutex
	plans map[string]stored
	now   func() time.Time
}

func newPlanStore() *planStore {
	return &planStore{plans: map[string]stored{}, now: time.Now}
}

// Put stamps and stores a plan under its hash, returning the hash.
func (s *planStore) Put(entry *stored) string {
	hash := Hash(entry.Org, entry.Mode, entry.Plan)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evict()
	entry.CreatedAt = s.now()
	s.plans[hash] = *entry

	return hash
}

// Get returns a stored plan if it exists and has not expired.
func (s *planStore) Get(hash string) (stored, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.plans[hash]
	if !ok || s.now().Sub(entry.CreatedAt) > planTTL {
		return stored{}, false
	}

	return entry, true
}

// evict drops expired plans. Called under the lock.
func (s *planStore) evict() {
	for hash, entry := range s.plans {
		if s.now().Sub(entry.CreatedAt) > planTTL {
			delete(s.plans, hash)
		}
	}
}
