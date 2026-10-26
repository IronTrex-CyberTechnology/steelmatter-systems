# Build image for Anthropic OSS Scanner (https://github.com/anthropics/oss-scanner).
# The scanner builds this with the repository as context, then runs the image with NO network, so every toolchain,
# module and npm dependency the tests need is fetched here. Toolchain pins follow versions.env; keep them in sync.
#
# Inside the image:
#   sm-pg-start      start the bundled PostgreSQL on 127.0.0.1:5432 (STEELMATTER_TEST_ADMIN_URL is preset)
#   scripts/ci       every test phase and the aggregate gate, as in GitHub CI

# PostgreSQL 17.10, same digest as versions.env POSTGRES_IMAGE and .github/workflows/ci.yml.
FROM postgres:17.10@sha256:7958605b474b3d264a969cb3a123d6aa00ad1e1fe9da8a69984dabb704d93317

ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
ARG NODE_VERSION=26.11.0
ARG NODE_SHA256_AMD64=db6342d36ebdb3cbd72103d0ce5ccc528620f6c9df72a11f4ccb384bf1bef678
ARG NODE_SHA256_ARM64=81a3cca833d080354236b5411a49003db8848ee003b122e212b88038692f7a11

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git xz-utils gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

# Go and Node from their official archives, checked against pinned SHA-256 sums.
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) go_sum="$GO_SHA256_AMD64"; node_arch=x64;   node_sum="$NODE_SHA256_AMD64" ;; \
      arm64) go_sum="$GO_SHA256_ARM64"; node_arch=arm64; node_sum="$NODE_SHA256_ARM64" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; \
    echo "$go_sum  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    curl -fsSLo /tmp/node.txz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz"; \
    echo "$node_sum  /tmp/node.txz" | sha256sum -c -; \
    tar -C /usr/local --strip-components=1 -xJf /tmp/node.txz; \
    rm /tmp/go.tgz /tmp/node.txz; \
    /usr/local/go/bin/go version; node --version

# GOTOOLCHAIN=local: never try to download another toolchain (there is no network in the scan).
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH \
    GOTOOLCHAIN=local \
    PGDATA=/var/lib/steelmatter-pg \
    STEELMATTER_TEST_ADMIN_URL=postgres://postgres:scanner-disposable@127.0.0.1:5432/postgres?sslmode=disable \
    STEELMATTER_ADMIN_URL=postgres://postgres:scanner-disposable@127.0.0.1:5432/postgres?sslmode=disable

# A disposable cluster, outside the base image's VOLUME so it survives into the final image. Loopback only;
# password auth over TCP as in CI. The password is a throwaway for this offline image, not a secret.
RUN set -eux; \
    mkdir -p "$PGDATA" && chown postgres:postgres "$PGDATA"; \
    echo scanner-disposable > /tmp/pwfile && chown postgres /tmp/pwfile; \
    gosu postgres initdb -D "$PGDATA" -U postgres --pwfile=/tmp/pwfile --auth-local=trust --auth-host=scram-sha-256; \
    rm /tmp/pwfile; \
    echo "listen_addresses = '127.0.0.1'" >> "$PGDATA/postgresql.conf"; \
    printf '%s\n' '#!/bin/sh' \
      '# Start the bundled PostgreSQL if it is not already running.' \
      'set -e' \
      'gosu postgres pg_ctl -D "$PGDATA" status >/dev/null 2>&1 && exit 0' \
      'gosu postgres pg_ctl -D "$PGDATA" -l /tmp/postgres.log -w -t 60 start' \
      > /usr/local/bin/sm-pg-start; \
    chmod +x /usr/local/bin/sm-pg-start

# scanner contract: the checkout lives inside the image, at /src
COPY . /src
WORKDIR /src

# Go modules (downloaded and verified) and synthetic signing keys. Without STEELMATTER_ADMIN_URL it skips the dev
# database, which needs PostgreSQL running.
RUN env -u STEELMATTER_ADMIN_URL scripts/bootstrap

# npm dependencies for the verifier, SDK, MCP adapter and demo, installed from lockfiles with lifecycle
# scripts disabled, the same way the test scripts do. Also builds the SDK the MCP adapter links to.
RUN bash -c 'source scripts/lib.sh && ensure_demo_deps && ensure_mcp_ready'

RUN go build ./... && go build -o /usr/local/bin/ ./cmd/...

# Run the full CI once so the build and test caches are warm and the image is known to work offline. A failing
# test must not abort the build.
RUN sm-pg-start \
    && (scripts/ci || echo "WARNING: scripts/ci reported failures"); \
    gosu postgres pg_ctl -D "$PGDATA" -m fast stop || true
