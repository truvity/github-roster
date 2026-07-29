package mapping_test

import (
	"testing"

	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/mapping/mappingtest"
)

func TestMemoryStore(t *testing.T) {
	t.Parallel()

	mappingtest.StoreSuite(t, func(*testing.T) mapping.Store { return mapping.NewMemory() })
}
