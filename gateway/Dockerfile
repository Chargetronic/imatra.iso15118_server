# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Gateway of the deployment: Caddy with the rate-limit module (mholt/caddy-ratelimit, Apache 2.0).
FROM caddy:2.11-builder AS build
# golang.org/x/net raised above Caddy's own requirement: CVE-2026-78661 (x/net/html,
# not used by this configuration) is fixed in 0.62.0. Drop the pin once Caddy requires it.
RUN xcaddy build --with github.com/mholt/caddy-ratelimit --replace golang.org/x/net=golang.org/x/net@v0.62.0
FROM caddy:2.11-alpine
# the base image's packages with their latest fixes (openssl, musl, curl, ...)
RUN apk upgrade --no-cache
COPY --from=build /usr/bin/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
