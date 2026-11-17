package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/store"
)

// The published spec lists the values of `provenance.sourceId`. A client that
// validates against the list rejects an event whose source is not on it, so
// every source an event can name must be published before the server can
// send it. This was found by hand once (three new sources, the whole of
// three layers rejected by the SDK); it is a test now.
func TestEveryEventSourceIsPublished(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "grid", "v1", "openapi.json"))
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(raw, &spec))
	at := func(m any, keys ...string) any {
		for _, k := range keys {
			obj, _ := m.(map[string]any)
			m = obj[k]
		}
		return m
	}
	published := map[string]bool{}
	values, _ := at(spec, "components", "schemas", "v1Provenance", "properties", "sourceId", "enum").([]any)
	for _, v := range values {
		if id, ok := v.(string); ok {
			published[id] = true
		}
	}
	require.NotEmpty(t, published, "the spec lists provenance.sourceId values")

	// Every feed the registry knows by name, and the Grid's own row. Reporter
	// rows are health only: no event names one.
	for id := range gridSourceInfo {
		assert.True(t, published[id], "source %q can be an event's provenance.sourceId but is not in the published list (api/grid/v1/grid.proto, Provenance.source_id)", id)
	}
	assert.True(t, published[store.GridSourceID], "the grid source owns the SOURCE_HEALTH events")
}
