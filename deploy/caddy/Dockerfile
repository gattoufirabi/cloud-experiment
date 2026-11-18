# Caddy with open-source modules:
#   - Coraza WAF + OWASP Core Rule Set (github.com/corazawaf/coraza-caddy, Apache-2.0)
#   - rate limiting per visitor IP (github.com/mholt/caddy-ratelimit, Apache-2.0)
#   - HTTP cache for the per-service edge cache (github.com/caddyserver/cache-handler, Apache-2.0)
#   - layer 4 (TCP) proxy for SMTP submission on port 465 (github.com/mholt/caddy-l4, Apache-2.0)
# Versions are pinned; bump them deliberately and rerun scripts/e2e-dev.sh.
FROM caddy:2.11-builder-alpine AS build
ENV GOTOOLCHAIN=auto
RUN xcaddy build v2.11.9 \
      --with github.com/corazawaf/coraza-caddy/v2@v2.6.2 \
      --with github.com/mholt/caddy-ratelimit@v0.1.0 \
      --with github.com/caddyserver/cache-handler@v0.16.0 \
      --with github.com/mholt/caddy-l4@v0.1.2

FROM caddy:2.11-alpine
COPY --from=build /usr/bin/caddy /usr/bin/caddy
RUN caddy list-modules | grep -q "http.handlers.waf" && caddy list-modules | grep -q "http.handlers.rate_limit" && caddy list-modules | grep -q "http.handlers.cache" && caddy list-modules | grep -q "layer4.handlers.proxy"
