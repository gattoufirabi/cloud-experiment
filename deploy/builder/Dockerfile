# Helper image for builds from git: git (clone) + Railpack (build plan for repositories
# without a Dockerfile). Runs as the first build step, in the gVisor sandbox.
# Railpack is MIT-licensed (github.com/railwayapp/railpack); the binary is pinned and
# verified against the checksum GitHub publishes for the release.
FROM alpine:3.20
ARG RAILPACK_VERSION=v0.43.0
ARG RAILPACK_SHA256=248a8f60f36854aabed6124113631078a802de3461660855fc4de9c26065c1fb
RUN apk add --no-cache git ca-certificates \
 && wget -q -O /tmp/railpack.tgz "https://github.com/railwayapp/railpack/releases/download/${RAILPACK_VERSION}/railpack-${RAILPACK_VERSION}-x86_64-unknown-linux-musl.tar.gz" \
 && echo "${RAILPACK_SHA256}  /tmp/railpack.tgz" | sha256sum -c - \
 && mkdir /tmp/rp && tar -xzf /tmp/railpack.tgz -C /tmp/rp \
 && install -m 0755 "$(find /tmp/rp -type f -name railpack | head -1)" /usr/local/bin/railpack \
 && rm -rf /tmp/rp /tmp/railpack.tgz \
 && railpack --version
# Railpack fetches its helper (mise) into /tmp/railpack on first use: do it once here
# and keep it in /opt/railpack-tmp; each build copies it into its own /tmp.
RUN mkdir -p /tmp/warm && echo '{"name":"warm","scripts":{"start":"node -e 0"}}' > /tmp/warm/package.json  && HOME=/tmp railpack prepare /tmp/warm --plan-out /tmp/warm/plan.json >/dev/null  && mkdir -p /opt/railpack-tmp && mv /tmp/railpack /opt/railpack-tmp/  && rm -rf /tmp/warm && chmod -R a+rX /opt/railpack-tmp
