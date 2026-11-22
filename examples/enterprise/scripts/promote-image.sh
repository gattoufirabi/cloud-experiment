#!/usr/bin/env bash
# Promotes an image tested in staging to the production organization's registry namespace.
# The layers and the digest stay the same: production runs exactly what staging tested.
#
#   REGISTRY=registry.example.com ORG=acme REGISTRY_TOKEN=plr_... \
#   PRODUCTION_ORG=acme-prod PRODUCTION_REGISTRY_TOKEN=plr_... ./promote-image.sh registry.example.com/acme/store:3f2c1a9
#
# With one organization for both environments (no PRODUCTION_ORG), the image is used as is.
# Prints the image reference to use as TF_VAR_production_image.
set -euo pipefail
SRC=${1:?usage: promote-image.sh <image tested in staging>}
if [ -z "${PRODUCTION_ORG:-}" ]; then echo "$SRC"; exit 0; fi
: "${REGISTRY:?}" "${ORG:?}" "${REGISTRY_TOKEN:?}" "${PRODUCTION_REGISTRY_TOKEN:?}"
DST="$REGISTRY/$PRODUCTION_ORG/${SRC#"$REGISTRY/$ORG/"}"

printf '%s' "$REGISTRY_TOKEN" | docker login "$REGISTRY" -u "$ORG" --password-stdin >/dev/null
docker pull -q "$SRC" >&2
want=$(docker inspect --format '{{index .RepoDigests 0}}' "$SRC" | cut -d@ -f2)
docker tag "$SRC" "$DST"
printf '%s' "$PRODUCTION_REGISTRY_TOKEN" | docker login "$REGISTRY" -u "$PRODUCTION_ORG" --password-stdin >/dev/null
docker push -q "$DST" >&2
got=$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$DST" | grep "/$PRODUCTION_ORG/" | cut -d@ -f2 | head -1)
[ "$want" = "$got" ] || { echo "digest changed while promoting ($want -> $got)" >&2; exit 1; }
echo "$DST"
