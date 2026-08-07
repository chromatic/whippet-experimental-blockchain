package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// //go:embed web/* will be added by the build system to embed static files.
// For now, we declare it as an empty embed to allow the code to compile.
// The Makefile will copy frontend assets to web/ and go build will pick them up.
//
//go:embed web/*
var webFS embed.FS

// contentTypeForPath returns the appropriate Content-Type for a file path.
func contentTypeForPath(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(p, ".webmanifest"):
		// The registered manifest media type (RFC 9412 / the Web App Manifest
		// spec). Browsers tolerate application/json too, but installability
		// checks (notably Chromium's) look for this specific type, so serving
		// the wrong one risks a manifest that loads fine yet is silently
		// ignored for "Add to Home Screen" purposes.
		return "application/manifest+json; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml; charset=utf-8"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(p, ".gif"):
		return "image/gif"
	case strings.HasSuffix(p, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// hasFileExtension returns true if the path appears to have a file extension.
func hasFileExtension(p string) bool {
	base := path.Base(p)
	// Remove leading dots (hidden files)
	base = strings.TrimPrefix(base, ".")
	// If there's a dot after removing leading dots, it has an extension
	return strings.Contains(base, ".")
}

// serveWeb creates an HTTP handler that serves static web files from the embedded filesystem.
// For paths with file extensions, it returns the file if found, or 404.
// For paths without extensions, it returns index.html (SPA fallback).
//
// This also determines the service worker's scope: a worker's default scope
// is the directory of its own URL, and web/sw.js is served flat at /sw.js
// (it has a .js extension, so the fallback branch above never substitutes
// index.html for it), which puts its default scope at "/" -- the whole
// origin. A worker served from, say, /assets/sw.js could only ever control
// requests under /assets/, leaving the rest of the app -- including the API
// calls this worker exists to intercept for the offline fallback -- outside
// its reach. Keeping sw.js at the top level is load-bearing, not cosmetic.
func serveWeb() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Ensure the path starts with /
		filePath := r.URL.Path
		if !strings.HasPrefix(filePath, "/") {
			filePath = "/" + filePath
		}
		// Remove leading slash for fs.FS lookup
		filePath = strings.TrimPrefix(filePath, "/")

		// Root should serve index.html
		if filePath == "" || filePath == "/" {
			filePath = "web/index.html"
		} else {
			// Prefix with web/ for embedded fs
			if !strings.HasPrefix(filePath, "web/") {
				filePath = "web/" + filePath
			}
		}

		// Try to read the file
		data, err := fs.ReadFile(webFS, filePath)
		if err != nil {
			// If the requested path has a file extension, return 404
			if hasFileExtension(r.URL.Path) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// If no extension, return index.html (SPA fallback)
			data, err = fs.ReadFile(webFS, "web/index.html")
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		// Set content type based on file extension
		ct := contentTypeForPath(filePath)
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// securityHeadersMiddleware wraps an HTTP handler to add security headers.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Security-Policy: restrict to same-origin for scripts/styles
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Don't send referrer to external sites
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
