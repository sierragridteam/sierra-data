package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A page on another origin (the Grid explorer, a consuming site) can use the
// API's validators: the preflight a script's If-None-Match triggers is
// answered with that header allowed, the response exposes ETag to the script,
// and the 304 carries the CORS headers a browser needs to accept it. Checked
// on every kind of route that sends an ETag, through a real Grid process and
// the repository's prefab.yaml (prefab finds it from the test's directory),
// because each kind is mounted on its own: gateway RPCs, the hand-built
// .geojson layers on the gateway mux, the landing files and the /mcp page.
//
// Until 2026-11-20 the preflight listed only Content-Type, X-Requested-With
// and X-Csrf-Protection and nothing was exposed, so a script could neither
// read a tag nor send one.
func TestConditionalGetFromAnotherOrigin(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	seedPlaceAndEvent(t, dbURL)
	a, port := preparedGrid(t, dbURL)
	h := runGrid(t, a)
	require.Equal(t, http.StatusOK, httpCode(t, port, "/readyz"))

	const origin = "https://grid.sierragridteam.org"
	routes := []struct {
		path   string
		accept string
	}{
		{path: "/api/v1/events"},                                 // gateway RPC, etag plugin
		{path: "/api/v1/places"},                                 // gateway RPC, etag plugin
		{path: "/api/v1/places/etag-test/map/mesh_node.geojson"}, // hand-built, on the gateway mux
		{path: "/api/v1/mesh/links"},                             // hand-built, on the gateway mux
		{path: "/"},                                              // landing page
		{path: "/llms.txt"},                                      // landing file
		{path: "/changelog"},                                     // rendered changelog
		{path: "/mcp", accept: "text/html"},                      // setup page at the MCP endpoint
	}
	for _, route := range routes {
		path := route.path
		headers := map[string]string{"Origin": origin}
		if route.accept != "" {
			headers["Accept"] = route.accept
		}

		// The preflight for a GET that carries If-None-Match.
		pre := send(t, http.MethodOptions, port, path, map[string]string{
			"Origin":                         origin,
			"Access-Control-Request-Method":  "GET",
			"Access-Control-Request-Headers": "if-none-match",
		})
		require.Less(t, pre.code, 300, "%s: preflight status", path)
		assert.Equal(t, "*", pre.header.Get("Access-Control-Allow-Origin"), "%s: preflight origin", path)
		assert.True(t, listHas(pre.header, "Access-Control-Allow-Headers", "If-None-Match"),
			"%s: the preflight must allow If-None-Match, got %q", path, pre.header.Values("Access-Control-Allow-Headers"))
		// Opening a request header must not open a method: GET only is what
		// keeps /mcp and the ingest route unreachable from a page.
		assert.Equal(t, "GET", pre.header.Get("Access-Control-Allow-Methods"), "%s: preflight methods", path)

		// The first read: the script must be able to see the tag.
		first := send(t, http.MethodGet, port, path, headers)
		require.Equal(t, http.StatusOK, first.code, "%s: %s", path, first.body)
		tag := first.header.Get("ETag")
		require.NotEmpty(t, tag, "%s: ETag", path)
		assertReadableCrossOrigin(t, path, first.header)

		// The revalidation: a 304 the browser accepts, with the tag readable.
		headers["If-None-Match"] = tag
		again := send(t, http.MethodGet, port, path, headers)
		require.Equal(t, http.StatusNotModified, again.code, "%s: If-None-Match", path)
		assert.Empty(t, again.body, "%s: a 304 has no body", path)
		assert.Equal(t, tag, again.header.Get("ETag"), "%s: the 304 repeats the tag", path)
		assertReadableCrossOrigin(t, path, again.header)
	}

	// A closed admission gate answers 503 with Retry-After on the hand-mounted
	// routes; a page must be able to read how long to wait.
	a.gate.Close()
	busy := send(t, http.MethodGet, port, "/", map[string]string{"Origin": origin})
	require.Equal(t, http.StatusServiceUnavailable, busy.code)
	assert.Equal(t, "1", busy.header.Get("Retry-After"))
	assertReadableCrossOrigin(t, "/ (closed gate)", busy.header)

	assert.Equal(t, 0, h.terminate())
}

// assertReadableCrossOrigin checks the headers of an actual (not preflight)
// response: any origin may read it, and ETag and Retry-After are exposed to
// the script. The allowed origin is the constant "*", never the caller's
// Origin echoed back, so the response does not depend on Origin and must not
// say "Vary: Origin" (it would split shared caches for nothing).
func assertReadableCrossOrigin(t *testing.T, path string, h http.Header) {
	t.Helper()
	assert.Equal(t, "*", h.Get("Access-Control-Allow-Origin"), "%s: allowed origin", path)
	for _, name := range []string{"ETag", "Retry-After"} {
		assert.True(t, listHas(h, "Access-Control-Expose-Headers", name),
			"%s: %s must be exposed, got %q", path, name, h.Values("Access-Control-Expose-Headers"))
	}
	assert.False(t, listHas(h, "Vary", "Origin"), "%s: Vary: Origin with a constant allowed origin", path)
}

// listHas reports whether the comma-separated header `name` lists `want`
// (header names and Vary members compare case-insensitively).
func listHas(h http.Header, name, want string) bool {
	for _, line := range h.Values(name) {
		for _, item := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(item), want) {
				return true
			}
		}
	}
	return false
}

func send(t *testing.T, method string, port int, path string, headers map[string]string) resp {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return resp{code: r.StatusCode, header: r.Header, body: string(body)}
}
