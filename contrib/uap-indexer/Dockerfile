# Multi-stage build: compile a static binary, then ship it with nothing
# else. The final image is FROM scratch -- no shell, no package manager,
# no libc, nothing an attacker could use beyond the binary itself. This
# is the whole point of running a small, dependency-free Go service: the
# container can be just as small and dependency-free.

FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
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
ENV UAP_STATEFILE=/data/uap-index.json

ENTRYPOINT ["/uap-indexer"]
CMD []
