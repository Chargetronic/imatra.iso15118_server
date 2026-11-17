#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Prepares a fresh Ubuntu host (24.04 or later) for the load test: Docker and
# Compose, kernel and file limits for ten thousand connections, time sync.
# Run once with sudo:  sudo loadtest/host-prepare.sh
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q ca-certificates curl git jq python3 chrony sysstat openssl tmux
command -v docker >/dev/null || apt-get install -y -q docker.io docker-compose-v2

# one TCP connection per device, plus reconnect waves
cat > /etc/sysctl.d/90-imatra-loadtest.conf <<'SYS'
fs.file-max = 2097152
fs.nr_open = 2097152
net.core.somaxconn = 8192
net.core.netdev_max_backlog = 16384
net.ipv4.tcp_max_syn_backlog = 8192
net.ipv4.ip_local_port_range = 10000 65000
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 15
vm.swappiness = 10
vm.overcommit_memory = 1
SYS
sysctl --system >/dev/null
cat > /etc/security/limits.d/90-imatra-loadtest.conf <<'LIM'
*    soft nofile 1048576
*    hard nofile 1048576
LIM
mkdir -p /etc/docker
cat > /etc/docker/daemon.json <<'DJ'
{
  "default-ulimits": { "nofile": { "Name": "nofile", "Soft": 1048576, "Hard": 1048576 } },
  "log-driver": "json-file",
  "log-opts": { "max-size": "50m", "max-file": "3" }
}
DJ
systemctl restart docker
systemctl enable --now chrony sysstat >/dev/null 2>&1 || true
echo "host ready: $(nproc) vCPU, $(free -g | awk '/Mem:/{print $2}') GB RAM, docker $(docker version -f '{{.Server.Version}}')"
echo "time sync: $(chronyc tracking 2>/dev/null | awk -F': ' '/System time/{print $2}')"
