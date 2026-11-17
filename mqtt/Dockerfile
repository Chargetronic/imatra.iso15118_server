# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

FROM eclipse-mosquitto:2
COPY mosquitto.conf /mosquitto/config/mosquitto.conf
COPY mqtt-init.sh /usr/local/bin/mqtt-init.sh
COPY mqtt-run.sh /usr/local/bin/mqtt-run.sh
RUN chmod 0755 /usr/local/bin/mqtt-init.sh /usr/local/bin/mqtt-run.sh && mkdir -p /mosquitto/conf.d
# Broker + reload on new passwords / ACL / block list (see mqtt-run.sh).
CMD ["/usr/local/bin/mqtt-run.sh"]
