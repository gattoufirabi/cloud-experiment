#!/usr/bin/env bash
# Weekly registry garbage collection: removes image layers no tag points to any more
# (the agent deletes old tags daily). The registry must not accept pushes while it
# runs, so it is stopped for about a minute. Run by platform-registry-gc.timer.
set -euo pipefail
DIR=${PLATFORM_DIR:-/opt/platform}
cd "$DIR/deploy"
before=$(du -sh /srv/platform/registry 2>/dev/null | cut -f1)
docker compose -f compose.prod.yml stop registry
trap 'docker compose -f compose.prod.yml start registry' EXIT
docker compose -f compose.prod.yml run --rm --no-deps --entrypoint /bin/sh registry -c \
  'sed "s|__EVENTS_SECRET__|unused|" /etc/registry/config.tmpl.yml > /tmp/config.yml && registry garbage-collect --delete-untagged /tmp/config.yml' \
  | grep -cE 'blob eligible for deletion|deleting' || true
after=$(du -sh /srv/platform/registry 2>/dev/null | cut -f1)
echo "registry storage: $before -> $after"
