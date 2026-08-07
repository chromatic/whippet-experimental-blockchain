package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIPathsMovedUnder /api/ verifies that the old unprefixed paths still work
// (backward compatibility) and that new /api/ prefixed paths work.
func TestAPIPathsMovedUnderAPI(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	tests := []struct {
		path string
		name string
	}{
		{"/api/status", "API /status"},
		{"/api/positions", "API /positions"},
		{"/api/utxos", "API /utxos"},
		{"/api/tokens", "API /tokens"},
		{"/api/orders", "API /orders"},
		{"/api/broadcast", "API /broadcast"},
		{"/api/feerate", "API /feerate"},
		// Old paths still work
		{"/status", "old /status"},
		{"/positions", "old /positions"},
		{"/utxos", "old /utxos"},
		{"/tokens", "old /tokens"},
		{"/orders", "old /orders"},
		{"/broadcast", "old /broadcast"},
		{"/feerate", "old /feerate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.RemoteAddr = "127.0.0.1:8000"
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)
			// Should not be 404 for these paths
			if w.Code == http.StatusNotFound {
				t.Errorf("%s returned 404, expected to be handled", tt.path)
			}
		})
	}
}

// TestSecurityHeadersPresent verifies that all responses include security headers.
func TestSecurityHeadersPresent(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.RemoteAddr = "127.0.0.1:8000"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	headers := w.Header()

	// Check for CSP header
	csp := headers.Get("Content-Security-Policy")
	if csp == "" {
		t.Error("missing Content-Security-Policy header")
	}
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP does not contain default-src 'self': %s", csp)
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP does not contain script-src 'self': %s", csp)
	}
	if !strings.Contains(csp, "object-src 'none'") {
		t.Errorf("CSP does not contain object-src 'none': %s", csp)
	}

	// Check X-Content-Type-Options
	xContentType := headers.Get("X-Content-Type-Options")
	if xContentType != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got: %s", xContentType)
	}

	// Check Referrer-Policy
	referrer := headers.Get("Referrer-Policy")
	if referrer != "no-referrer" {
		t.Errorf("expected Referrer-Policy: no-referrer, got: %s", referrer)
	}
}

// TestStaticFilesServedFromRoot verifies that index.html is served at /
// and that CSS/JS files have correct Content-Type.
func TestStaticFilesServedFromRoot(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	tests := []struct {
		path        string
		expectCode  int
		expectCType string
		name        string
	}{
		{"/", http.StatusOK, "text/html", "root serves index.html"},
		{"/index.html", http.StatusOK, "text/html", "index.html"},
		{"/style.css", http.StatusOK, "text/css", "style.css has text/css"},
		{"/app.js", http.StatusOK, "application/javascript", "app.js has application/javascript"},
		{"/unknown.txt", http.StatusNotFound, "", "unknown file returns 404"},
		{"/notafile", http.StatusOK, "text/html", "path without ext serves index.html (SPA fallback)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)
			if w.Code != tt.expectCode {
				t.Errorf("expected %d, got %d", tt.expectCode, w.Code)
			}
			if tt.expectCType != "" {
				ct := w.Header().Get("Content-Type")
				if !strings.HasPrefix(ct, tt.expectCType) {
					t.Errorf("expected Content-Type %s, got %s", tt.expectCType, ct)
				}
			}
		})
	}
}

// TestPWAAssetsServed verifies that the manifest, service worker, its
// registration script, and the icon it references are all actually served
// out of the embedded FS with the content types that make them functional --
// not just present as files on disk. A manifest served as
// application/octet-stream is a manifest most browsers silently refuse to
// install from, and a service worker not served from "/" cannot ever
// register at the scope this wallet needs (see the comment on serveWeb in
// web.go), so both are worth pinning down explicitly rather than trusting
// that "the file exists" is the same as "the feature works".
func TestPWAAssetsServed(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	tests := []struct {
		path        string
		expectCType string
		name        string
	}{
		{"/manifest.webmanifest", "application/manifest+json", "manifest served with manifest content type"},
		{"/sw.js", "application/javascript", "service worker served from root path (required for full-app scope)"},
		{"/sw-register.js", "application/javascript", "registration script served"},
		{"/icon.svg", "image/svg+xml", "manifest icon served"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			ct := w.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, tt.expectCType) {
				t.Errorf("expected Content-Type %s, got %s", tt.expectCType, ct)
			}
			if w.Body.Len() == 0 {
				t.Errorf("expected non-empty body for %s", tt.path)
			}
		})
	}
}

// TestManifestLinkedFromIndex verifies index.html actually references the
// manifest and registers the service worker. This is the test that would
// catch someone shipping manifest.webmanifest and sw.js as orphan files that
// no page ever links to -- which serves fine, installs nothing, and looks
// identical to success from TestPWAAssetsServed alone.
func TestManifestLinkedFromIndex(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `rel="manifest"`) || !strings.Contains(body, "manifest.webmanifest") {
		t.Error("index.html does not link the PWA manifest")
	}
	if !strings.Contains(body, "sw-register.js") {
		t.Error("index.html does not load the service worker registration script")
	}
}

// TestServiceWorkerNeverCachesKeyMaterial is a static guard against the one
// mistake that would matter most here: this is a self-custody wallet, and a
// service worker is a second, independent place private-key material could
// leak into persistent storage if someone "helpfully" widened its caching
// rules later. wallet.js never persists the mnemonic or private key (see its
// own "Persistence rule" comment) and app.js backs the wallet with a plain
// in-memory Map, so there is no localStorage/IndexedDB key material for a
// cache to pick up -- but this test pins the worker source itself so a
// future change can't quietly start caching an endpoint or storage API that
// would carry key data, without at least touching a very deliberately named
// test.
func TestServiceWorkerNeverCachesKeyMaterial(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /sw.js, got %d", w.Code)
	}
	src := w.Body.String()

	// Match actual API usage (a call or property access), not the mere word
	// appearing in an explanatory comment -- sw.js's own comments document
	// *why* it avoids these, which would otherwise trip a bare substring
	// check on itself.
	for _, forbidden := range []string{"localStorage.", "indexedDB.", "sessionStorage."} {
		if strings.Contains(src, forbidden) {
			t.Errorf("sw.js references %s; a service worker must never read/write wallet storage APIs", forbidden)
		}
	}
	// The worker must only ever intercept GET; POST /api/broadcast (and any
	// other mutating call) has to reach the network untouched every time.
	if !strings.Contains(src, `req.method !== 'GET'`) {
		t.Error("sw.js does not appear to exclude non-GET requests from interception")
	}
}
