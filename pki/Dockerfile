# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

FROM alpine:3.24
RUN apk upgrade --no-cache && apk add --no-cache openssl
COPY pki-init.sh preflight.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/pki-init.sh /usr/local/bin/preflight.sh
ENTRYPOINT ["/usr/local/bin/pki-init.sh"]
