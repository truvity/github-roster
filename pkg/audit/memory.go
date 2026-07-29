package audit

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
)

// Compile-time interface check.
var _ Sink = (*Memory)(nil)

// Memory keeps records in process.
//
// For local runs and for tests of everything above the sink. It is
// deliberately NOT a fallback: a deployment that cannot reach its bucket
// gets an error, because silently keeping an audit trail in memory — where
// it dies with the pod — is worse than having none, since it looks like one.
type Memory struct {
	mu      sync.RWMutex
	records []Record
}

// NewMemory returns an empty in-process sink.
func NewMemory() *Memory { return &Memory{} }

// Write stores a record.
func (m *Memory) Write(_ context.Context, record Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.records = append(m.records, record)

	return nil
}

// List returns records newest first.
func (m *Memory) List(_ context.Context, org string, limit int) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 {
		limit = DefaultLimit
	}

	out := make([]Record, 0, len(m.records))

	for i := range m.records {
		if org == "" || m.records[i].Org == org {
			out = append(out, m.records[i])
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

// bytesReader is here rather than in s3.go so both files share one helper.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
