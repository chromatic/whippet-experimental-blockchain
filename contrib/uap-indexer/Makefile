.PHONY: web check-web build dist size test clean help

# Default target
all: build

# Binary output name
BINARY = uap-indexer

# Maximum size in bytes (6 MB) of the *shipped* artifact, which is the
# UPX-compressed one. The uncompressed build is around 11 MB, most of it
# the pure-Go SQLite engine; measuring that number would only be measuring
# something nobody deploys.
MAX_SIZE = 6291456

# Name of the UPX-compressed artifact that 'dist' produces from $(BINARY).
# Kept as a separate file rather than compressing $(BINARY) in place: UPX
# rewrites the file it's given and refuses to touch an already-packed one,
# which used to make a second 'make dist' fail outright, and it also meant
# 'make build' could hand back either a plain or a UPX-packed binary
# depending on what had run before it. Compressing to a copy keeps $(BINARY)
# always the ordinary, uncompressed build.
DIST_BINARY = $(BINARY).upx

help:
	@echo "Targets:"
	@echo "  web       - Copy frontend assets from contrib/uap-web (+ uap-js) to web/"
	@echo "  check-web - Verify every frontend import resolves inside web/"
	@echo "  build     - Build the binary (depends on web)"
	@echo "  dist      - Build \$(BINARY) and UPX-compress a copy to \$(DIST_BINARY) for shipping"
	@echo "  size      - Check the compressed artifact's size and fail if > 6 MB"
	@echo "  test      - Run all tests"
	@echo "  clean     - Remove web/ directory and built binaries"

# Copy frontend assets from contrib/uap-web (and the uap-js library it imports)
# into web/, which web.go embeds with //go:embed web/*.
#
# Layout matters here. In the source tree uap-js is a *sibling* of uap-web, and
# uap-web's modules import it as '../uap-js/uap.js'. A browser resolves that
# against the URL of the importing module, clamping leading '..' at the document
# root: /app.js importing '../uap-js/uap.js' requests /uap-js/uap.js. So the
# library has to land in web/uap-js/ for those imports to resolve once served.
# 'make check-web' verifies exactly that.
#
# The elliptic-curve library rides along the same way. uap-web used to import
# the bare specifier '@noble/secp256k1', which node resolves through
# node_modules but a browser cannot resolve at all without an import map -- and
# an inline import map is blocked by this service's `script-src 'self'` CSP.
# So uap-js vendors the library as noble-secp256k1.js and wraps it in secp.js,
# and both are plain files that this copy step picks up like any other module.
# UAP_JS_REQUIRED asserts they arrived: without them the wallet loads and then
# dies on its first import, which is not a failure mode worth shipping.
#
# Only production modules are copied: no test files, no test harnesses.
UAP_JS_EXCLUDE = *.test.js test-helpers.js test-node-harness.js integration-helpers.js
UAP_JS_REQUIRED = uap.js addr.js sha256.js ripemd160.js secp.js noble-secp256k1.js

# PWA assets uap-web's index.html links to: the manifest, the service
# worker and its registration script, and the icon the manifest points at.
# None of these are .html/.css/.js so the glob below has to name them
# explicitly, and UAP_WEB_REQUIRED exists for the same reason
# UAP_JS_REQUIRED does above -- a manifest that 404s, or a service worker
# that never arrives, is a silent failure in the browser (the page still
# loads, it just never installs/goes offline) rather than a loud one, and
# a silent failure is not a failure mode worth shipping.
UAP_WEB_REQUIRED = manifest.webmanifest icon.svg sw.js sw-register.js

web:
	@echo "Copying frontend assets from ../uap-web to web/"
	@mkdir -p web web/uap-js
	@# Static assets and production JS from uap-web (skip *.test.js and harnesses)
	@for file in ../uap-web/*.html ../uap-web/*.css ../uap-web/*.js ../uap-web/*.webmanifest ../uap-web/*.svg; do \
		[ -f "$$file" ] || continue; \
		case "$${file##*/}" in \
			*.test.js|test-harness.js) continue;; \
		esac; \
		cp "$$file" web/; \
	done
	@for req in $(UAP_WEB_REQUIRED); do \
		if [ ! -f "web/$$req" ]; then \
			echo "✗ web/$$req missing after copy; the frontend will 404 on it"; \
			exit 1; \
		fi; \
	done
	@# The uap-js library that uap-web imports across the directory boundary
	@if [ ! -d ../uap-js ]; then \
		echo "✗ ../uap-js not found; frontend imports of '../uap-js/*.js' will 404"; \
		exit 1; \
	fi
	@for file in ../uap-js/*.js; do \
		[ -f "$$file" ] || continue; \
		skip=; \
		for pat in $(UAP_JS_EXCLUDE); do \
			case "$${file##*/}" in $$pat) skip=1;; esac; \
		done; \
		if [ -z "$$skip" ]; then cp "$$file" web/uap-js/; fi; \
	done
	@for req in $(UAP_JS_REQUIRED); do \
		if [ ! -f "web/uap-js/$$req" ]; then \
			echo "✗ web/uap-js/$$req missing after copy; the frontend will 404 on it"; \
			echo "  (if it is noble-secp256k1.js: cd ../uap-js && npm install && npm run vendor:secp)"; \
			exit 1; \
		fi; \
	done
	@echo "✓ Frontend assets copied to web/"

# Verify that every module import specifier under web/ actually resolves against
# what was copied there. Catches the class of breakage where uap-web grows a new
# cross-directory or bare import that the copy step above does not account for.
check-web: web
	@command -v node >/dev/null 2>&1 || { \
		echo "✗ node not found; cannot run the frontend import check"; \
		exit 1; \
	}
	@node check-web-imports.mjs web

# Build the binary (depends on web)
# Use -ldflags="-s -w" to strip symbols and dwarf info for a smaller binary,
# and -trimpath so absolute build-machine paths (source file paths, GOPATH)
# don't end up embedded in it either -- matches what the Dockerfile does,
# so a local build and a container build produce equivalent binaries.
build: web
	@echo "Building $(BINARY)..."
	@go build -trimpath -ldflags="-s -w" -o $(BINARY)
	@echo "✓ Built $(BINARY)"

# Produce the UPX-compressed artifact that actually ships, as a copy of
# $(BINARY) rather than an in-place rewrite. Remove any stale copy first so
# 'make dist' is idempotent -- 'upx -q' will happily create $(DIST_BINARY)
# fresh each time, but leaving an old packed file around from a previous,
# now-stale build would be worse than starting clean. UPX roughly halves
# the size; the process self-extracts into memory at startup, which costs
# a few tens of milliseconds once, against a service that then runs for
# weeks.
dist: build
	@command -v upx >/dev/null 2>&1 || { \
		echo "✗ upx not found; install it or build with 'make build' and skip the size check"; \
		exit 1; \
	}
	@echo "Compressing $(BINARY) -> $(DIST_BINARY)..."
	@rm -f $(DIST_BINARY)
	@cp $(BINARY) $(DIST_BINARY)
	@upx -q $(DIST_BINARY) >/dev/null
	@echo "✓ Compressed $(DIST_BINARY)"

# Check the size of what actually ships (the UPX-compressed artifact), and
# report the uncompressed size alongside it for context -- both numbers
# come from files 'dist' already built, so there's nothing extra to compute.
size: dist
	@RAW_SIZE=$$(stat -f%z $(BINARY) 2>/dev/null || stat -c%s $(BINARY) 2>/dev/null); \
	SIZE=$$(stat -f%z $(DIST_BINARY) 2>/dev/null || stat -c%s $(DIST_BINARY) 2>/dev/null); \
	RAW_SIZE_MB=$$(echo "scale=2; $$RAW_SIZE / 1048576" | bc); \
	SIZE_MB=$$(echo "scale=2; $$SIZE / 1048576" | bc); \
	echo "  Uncompressed: $$RAW_SIZE_MB MB ($(BINARY))"; \
	if [ $$SIZE -gt $(MAX_SIZE) ]; then \
		echo "✗ Compressed size $$SIZE bytes ($$SIZE_MB MB, $(DIST_BINARY)) exceeds limit of $(MAX_SIZE) bytes"; \
		exit 1; \
	else \
		echo "✓ Compressed size: $$SIZE_MB MB ($(DIST_BINARY), within $(MAX_SIZE) byte limit)"; \
	fi

# Run all tests
test:
	@echo "Running tests..."
	@go test -v -race ./...

# Clean up generated files
clean:
	@echo "Cleaning up..."
	@rm -rf web/
	@rm -f $(BINARY) $(DIST_BINARY)
	@echo "✓ Cleaned"
