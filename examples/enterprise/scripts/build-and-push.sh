#!/usr/bin/env bash
# Builds the store image once and pushes it to the platform's private registry, tagged with
# the git commit, so staging and production run byte-for-byte the same image.
#
#   REGISTRY=registry.example.com ORG=acme REGISTRY_TOKEN=plr_... ./build-and-push.sh
#
# REGISTRY_TOKEN is a registry token with push rights (console > Registry > Tokens).
# Prints the image reference to use as TF_VAR_image.
set -euo pipefail
cd "$(dirname "$0")/../../store/app"
: "${REGISTRY:?set REGISTRY (the platform registry host)}" "${ORG:?set ORG (your organization ID)}" "${REGISTRY_TOKEN:?set REGISTRY_TOKEN}"
TAG=${TAG:-$(git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M%S)}
IMAGE="$REGISTRY/$ORG/store:$TAG"

printf '%s' "$REGISTRY_TOKEN" | docker login "$REGISTRY" -u "$ORG" --password-stdin >/dev/null
docker build --pull -t "$IMAGE" . >&2
docker push "$IMAGE" >&2
# The platform scans pushed images; a policy may block images with critical findings.
echo "$IMAGE"
