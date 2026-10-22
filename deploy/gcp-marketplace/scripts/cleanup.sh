#!/bin/bash
# Strips build-time state so the published image carries nothing identifying
# and nothing secret.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get -y autoremove
apt-get clean
rm -rf /var/lib/apt/lists/*

# The guest agent regenerates host keys on first boot (set_host_keys above).
# A shipped host key would be the same key on every buyer's instance.
rm -f /etc/ssh/ssh_host_*

# Packer's build user and anything it left behind.
rm -rf /home/packer
rm -f /root/.bash_history /home/*/.bash_history
rm -rf /root/.ssh /home/*/.ssh
rm -rf /root/.config/gcloud /home/*/.config/gcloud
rm -f /root/.gsutil/credstore* 2>/dev/null || true
rm -rf /tmp/* /var/tmp/*

# Google guest agent state that identifies the build instance.
rm -rf /var/lib/google/* 2>/dev/null || true

find /var/log -type f -exec truncate -s 0 {} \;
truncate -s 0 /etc/machine-id   # systemd regenerates this per instance

# Keys and configuration are instance state, not image content: a shipped key
# would be a shipped credential, the same one on every launch.
rm -rf /var/lib/polign/* /var/cache/polign/* /var/lib/recall/*
rm -f /etc/recall/recall.env /etc/recall/*key*

history -c 2>/dev/null || true
sync
