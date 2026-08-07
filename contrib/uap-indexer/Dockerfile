# Multi-stage build: compile a static binary, then ship it with nothing
# else. The final image is FROM scratch -- no shell, no package manager,
# no libc, nothing an attacker could use beyond the binary itself. This
# is the whole point of running a small, dependency-free Go service: the
# container can be just as small and dependency-free.
#
# The build context is contrib/, not this directory. Two things this
# package depends on live outside uap-indexer/ and have to be visible to
# the build:
#   - go.mod's `replace github.com/whippet/whippet/contrib/whippetrpc =>
#     ../whippetrpc` needs a whippetrpc/ sibling directory, which a
#     context scoped to uap-indexer/ can never provide (COPY cannot reach
#     outside the build context).
#   - //go:embed web/* (web.go) needs web/, which `make web` generates by
#     copying files out of ../uap-web and ../uap-js -- also outside this
#     directory.
# contrib/ is the lowest common ancestor of all four, so build from there:
#
#   docker build -f uap-indexer/Dockerfile contrib/
#
# (run from the repository's contrib/ directory, or adjust the trailing
# path accordingly). Building with `docker build .` from inside
# uap-indexer/ will fail for the reasons above.

FROM golang:1.25-alpine AS build

# `make web` (invoked below) only copies files -- no node needed for that,
# only for `make check-web`, which this build does not run -- but the copy
# step itself is driven by this package's Makefile, so `make` has to be on
# the image. Alpine's base image doesn't include it.
RUN apk add --no-cache make

WORKDIR /src

# Bring in the whole contrib/ subtree the build needs: uap-indexer itself,
# whippetrpc (resolved via the go.mod replace directive above), and the
# uap-web/uap-js frontend sources that `make web` copies from. Layout is
# preserved as siblings under /src, matching the source tree, so the
# relative paths in go.mod's replace directive and in the Makefile's `web`
# target (../uap-web, ../uap-js) resolve exactly as they do outside Docker.
COPY uap-indexer/ ./uap-indexer/
COPY whippetrpc/ ./whippetrpc/
COPY uap-web/ ./uap-web/
COPY uap-js/ ./uap-js/

WORKDIR /src/uap-indexer

# Stage the frontend into web/ before compiling, since //go:embed web/*
# needs at least one file there and fails otherwise.
RUN make web

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/uap-indexer .

# An empty directory owned by the non-root UID/GID we'll run as, purely so
# the final image has something at /data with correct ownership *before*
# VOLUME is declared. This matters: when Docker creates a fresh named
# volume, it initializes it by copying whatever already exists in the
# image at that path -- including ownership. Skip this and a fresh volume
# comes up root-owned, which the non-root process below can never write
# to (state saves fail silently at every shutdown, discovered by actually
# restarting a running container and checking, not by reading the
# Dockerfile).
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch

# CA certificates, so outbound HTTPS to -mirror peers (if any use https://)
# actually verifies. Talking to your own whippetd is plain RPC over
# whatever network you've set up (see README) and doesn't need this.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/uap-indexer /uap-indexer
COPY --from=build --chown=65532:65532 /out/data /data

# Run as a fixed non-root numeric UID/GID. scratch has no /etc/passwd, but
# Linux doesn't require one for a numeric USER -- it just sets the UID
# directly on the process.
USER 65532:65532

# State file and any TLS material you might mount in (e.g. a cookie file)
# live here. Mount a volume at this path so state survives container
# restarts/recreation.
VOLUME ["/data"]

EXPOSE 8961

# Defaults set via ENV, not baked into CMD as explicit flags -- an
# explicit flag always overrides an env-derived default (see envOr() in
# main.go), so hardcoding them in CMD would silently defeat `docker run
# -e UAP_LISTEN=...`/`-e UAP_STATEFILE=...` overrides. 0.0.0.0 (not
# 127.0.0.1) so the default actually works with `docker run -p`, not just
# --network host.
ENV UAP_LISTEN=0.0.0.0:8961
ENV UAP_STATEFILE=/data/uap-index.sqlite

ENTRYPOINT ["/uap-indexer"]
CMD []
