package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// An omitted key must not drop the safety check; a negative one is an explicit
// opt-out and must survive verbatim (clamping it to the default would silently
// re-enable a check an operator turned off).
func TestMeshcoreSilence(t *testing.T) {
	assert.Equal(t, DefaultMeshcoreSilenceAfter, MeshcoreConfig{}.Silence())
	assert.Equal(t, 5*time.Minute, MeshcoreConfig{SilenceAfter: 5 * time.Minute}.Silence())
	assert.Equal(t, -time.Second, MeshcoreConfig{SilenceAfter: -time.Second}.Silence())
}

// A retired entry must be a full public key: a prefix could name a second
// node, and a skipped entry would leave the node on the map while the config
// says it is retired.
func TestMeshcoreRetiredKeys(t *testing.T) {
	full := "CA57B9577EE6EE8444FFAA632C1BCAAB62CD1CB3F8C0D0090B55C9DED65BBB25"
	keys, err := MeshcoreConfig{Retired: []MeshcoreRetiredNode{{Key: " " + full + " ", Name: "SIERRA Eagle One"}}}.RetiredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if !keys[strings.ToLower(full)] || len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	if keys, err := (MeshcoreConfig{}).RetiredKeys(); err != nil || keys != nil {
		t.Fatalf("no entries: %v, %v", keys, err)
	}
	for _, bad := range []string{"", "ca57b9577ee6ee84", full[:63] + "g", full + "00"} {
		if _, err := (MeshcoreConfig{Retired: []MeshcoreRetiredNode{{Key: bad, Name: "x"}}}).RetiredKeys(); err == nil {
			t.Errorf("key %q was accepted", bad)
		}
	}
}
