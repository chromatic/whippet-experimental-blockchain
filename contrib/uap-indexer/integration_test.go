package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCompleteIntegrationWithFrontendAndAPI verifies the full integrated server
// serving both frontend and API endpoints with security headers.
func TestCompleteIntegrationWithFrontendAndAPI(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	tests := []struct {
		path           string
		method         string
		expectCode     int
		expectCSP      bool
		expectXContent bool
		expectReferrer bool
		name           string
	}{
		// Frontend routes
		{"/", "GET", http.StatusOK, true, true, true, "root index.html"},
		{"/index.html", "GET", http.StatusOK, true, true, true, "explicit index.html"},
		{"/style.css", "GET", http.StatusOK, true, true, true, "stylesheet"},
		{"/app.js", "GET", http.StatusOK, true, true, true, "app module"},
		{"/unknown/path", "GET", http.StatusOK, true, true, true, "SPA fallback (no extension)"},
		{"/unknown.txt", "GET", http.StatusNotFound, true, true, true, "unknown file with extension"},

		// API routes - new /api/ prefixed paths
		{"/api/status", "GET", http.StatusOK, true, true, true, "/api/status"},
		{"/api/positions", "GET", http.StatusBadRequest, true, true, true, "/api/positions (no pubkey)"},
		{"/api/utxos", "GET", http.StatusBadRequest, true, true, true, "/api/utxos (no address)"},
		{"/api/tokens", "GET", http.StatusOK, true, true, true, "/api/tokens"},
		{"/api/orders", "GET", http.StatusOK, true, true, true, "/api/orders"},
		{"/api/feerate", "GET", http.StatusServiceUnavailable, true, true, true, "/api/feerate (no broadcaster)"},

		// API routes - old unprefixed paths (backward compatibility)
		{"/status", "GET", http.StatusOK, true, true, true, "old /status"},
		{"/positions", "GET", http.StatusBadRequest, true, true, true, "old /positions (no pubkey)"},
		{"/utxos", "GET", http.StatusBadRequest, true, true, true, "old /utxos (no address)"},
		{"/tokens", "GET", http.StatusOK, true, true, true, "old /tokens"},
		{"/orders", "GET", http.StatusOK, true, true, true, "old /orders"},
		{"/feerate", "GET", http.StatusServiceUnavailable, true, true, true, "old /feerate (no broadcaster)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.RemoteAddr = "127.0.0.1:8000"
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if w.Code != tt.expectCode {
				t.Errorf("expected status %d, got %d", tt.expectCode, w.Code)
			}

			headers := w.Header()

			if tt.expectCSP {
				csp := headers.Get("Content-Security-Policy")
				if csp == "" {
					t.Error("missing Content-Security-Policy header")
				}
			}

			if tt.expectXContent {
				xct := headers.Get("X-Content-Type-Options")
				if xct != "nosniff" {
					t.Errorf("expected X-Content-Type-Options: nosniff, got: %s", xct)
				}
			}

			if tt.expectReferrer {
				rp := headers.Get("Referrer-Policy")
				if rp != "no-referrer" {
					t.Errorf("expected Referrer-Policy: no-referrer, got: %s", rp)
				}
			}
		})
	}
}

// TestAPIPathPrefixDoesNotBreakContentType verifies that the /api/ prefixed paths
// still set appropriate Content-Type headers for responses.
func TestAPIPathPrefixDoesNotBreakContentType(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	tests := []struct {
		path        string
		expectCType string
		name        string
	}{
		{"/api/status", "application/json", "API JSON response"},
		{"/api/tokens", "application/json", "API JSON response"},
		{"/api/orders", "application/json", "API JSON response"},
		{"/status", "application/json", "old path JSON response"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.RemoteAddr = "127.0.0.1:8000"
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			ct := w.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, tt.expectCType) {
				t.Errorf("expected Content-Type starting with %s, got %s", tt.expectCType, ct)
			}
		})
	}
}

// TestSPAFallbackOnly500Errors verifies that index.html is only served as SPA
// fallback for paths without extensions, not for paths with extensions.
func TestSPAFallbackOnlyForPathsWithoutExtension(t *testing.T) {
	idx := NewIndex()
	server := newAPIServer(idx, nil, nil, false, nil)

	tests := []struct {
		path       string
		shouldFall bool
		name       string
	}{
		{"/wallet", true, "path without extension -> fallback to index.html"},
		{"/dashboard", true, "another path without extension -> fallback"},
		{"/unknown.js", false, "path with .js extension -> 404, not fallback"},
		{"/config.json", false, "path with .json extension -> 404, not fallback"},
		{"/data.txt", false, "path with .txt extension -> 404, not fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if tt.shouldFall {
				if w.Code != http.StatusOK {
					t.Errorf("expected SPA fallback (200), got %d", w.Code)
				}
				ct := w.Header().Get("Content-Type")
				if !strings.HasPrefix(ct, "text/html") {
					t.Errorf("expected text/html for fallback, got %s", ct)
				}
			} else {
				if w.Code != http.StatusNotFound {
					t.Errorf("expected 404 for path with extension, got %d", w.Code)
				}
			}
		})
	}
}
