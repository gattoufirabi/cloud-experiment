// Package buildsrc holds what the platform's builder (internal/components/builds/agent) and build servers
// (internal/components/servers/nodeagent) share: the source preparation scripts and the zip conversion.
package buildsrc

// The first build step runs one of these in the builder image (git + Railpack), in a
// fresh volume mounted at /src: it fetches the source into /src/repo, prints
// COMMIT=<sha>, then Common decides how to build and prints MODE=dockerfile|railpack.
//
// Exit codes: 1 clone failed, 2 folder not found, 3 Railpack could not plan the build.
const (
	// Git clones $BRANCH of $REPO (with $TOKEN for private repositories).
	Git = `set -eu
cd /src
URL="$REPO"
if [ -n "${TOKEN:-}" ]; then URL="${REPO%%://*}://x-access-token:${TOKEN}@${REPO#*://}"; fi
clone() { git clone --quiet --single-branch --branch "$BRANCH" "$@" -- "$URL" repo; }
if ! clone --depth 1 2>/tmp/err; then
  rm -rf repo
  if ! clone 2>>/tmp/err; then
    if [ -n "${TOKEN:-}" ]; then sed "s|${TOKEN}|***|g" /tmp/err >&2; else cat /tmp/err >&2; fi
    exit 1
  fi
fi
echo "COMMIT=$(git -C repo rev-parse HEAD)"
rm -rf repo/.git
`
	// Zip: the archive was already copied into /src/repo; $SHA is its SHA-256.
	Zip = `set -eu
cd /src
echo "COMMIT=$SHA"
`
	Common = `D="repo/${DIR:-.}"
if [ ! -d "$D" ]; then echo "Folder ${DIR} not found in the repository." >&2; exit 2; fi
mkdir -p out plan && chmod 777 out
if [ -f "$D/Dockerfile" ]; then
  echo "MODE=dockerfile"
else
  # No Dockerfile: Railpack detects the language and writes a build plan.
  cp -a /opt/railpack-tmp/. /tmp/ 2>/dev/null || true
  if ! railpack prepare "$D" --plan-out plan/railpack-plan.json --info-out plan/info.json >/tmp/prepare.out 2>&1; then
    cat /tmp/prepare.out >&2
    exit 3
  fi
  tail -n 30 /tmp/prepare.out
  echo "MODE=railpack"
fi
chmod -R a+rX repo plan
chown 1000:1000 /cache
# A pipeline (staysa.yml): its steps may run as any user and write to the workspace.
if [ -f "$D/staysa.yml" ]; then
  chmod -R a+rwX repo
  echo "PIPELINE=$(head -c 65537 "$D/staysa.yml" | base64 | tr -d '\n')"
fi`
)

// FailureMessage explains a failed first step to the user.
func FailureMessage(code int, zip bool, dir string) string {
	switch code {
	case 2:
		return "folder " + dir + " not found in the repository"
	case 3:
		return "no Dockerfile, and Railpack could not detect how to build this project (see the log above); add a Dockerfile"
	}
	if zip {
		return "could not unpack the archive"
	}
	return "could not clone the repository (check the address, branch and token)"
}

// BuilderDockerfile builds the helper image (git + Railpack) on a build server, which
// cannot pull the platform's locally built one. Keep it identical to
// deploy/builder/Dockerfile (a test checks).
const BuilderDockerfile = `# Helper image for builds from git: git (clone) + Railpack (build plan for repositories
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
`
