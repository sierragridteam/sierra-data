package main

import (
	"testing"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
)

// The committed grid.meshcore.retired list parses: a malformed key stops the
// server at startup (config.LoadConfig), so it must be caught here first.
func TestCommittedRetiredNodesAreFullKeys(t *testing.T) {
	var mc config.MeshcoreConfig
	require.NoError(t, prefab.Config.Unmarshal("grid.meshcore", &mc))
	require.NotEmpty(t, mc.Brokers, "the repository's prefab.yaml was not read")
	keys, err := mc.RetiredKeys()
	require.NoError(t, err)
	require.Len(t, keys, len(mc.Retired), "an entry is listed twice")
	for _, r := range mc.Retired {
		require.NotEmpty(t, r.Name, "retired entry %s has no name", r.Key)
		require.NotEmpty(t, r.Since, "retired entry %s (%s) has no date", r.Key, r.Name)
	}
}
