#!/bin/bash
# Strips build-time state so the published image carries nothing identifying
# and nothing secret.
set -euo pipefail

dnf clean all
rm -rf /var/cache/dnf/*

cloud-init clean --logs 2>/dev/null || true

rm -f /etc/ssh/ssh_host_*          # cloud-init regenerates these per instance
rm -rf /root/.ssh /home/ec2-user/.ssh
rm -f /root/.bash_history /home/ec2-user/.bash_history
rm -rf /root/.aws /home/ec2-user/.aws
rm -rf /tmp/* /var/tmp/*

find /var/log -type f -exec truncate -s 0 {} \;
truncate -s 0 /etc/machine-id

# Keys and configuration are instance state, not image content: a shipped key
# would be a shipped credential, the same one on every launch.
rm -rf /var/lib/polign/* /var/cache/polign/* /var/lib/recall/*
rm -f /etc/recall/recall.env /etc/recall/*key*

history -c 2>/dev/null || true
sync
