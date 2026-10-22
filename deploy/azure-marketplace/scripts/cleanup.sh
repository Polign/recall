#!/bin/bash
# Strips build-time state so the published image carries nothing identifying
# and nothing secret. waagent -deprovision+user runs after this as the final
# Packer step and handles host keys, DHCP leases, the hostname, and the packer
# account itself; this removes what the agent does not know about.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get -y autoremove
apt-get clean
rm -rf /var/lib/apt/lists/*

rm -f /root/.bash_history /home/*/.bash_history
rm -rf /root/.ssh
rm -rf /root/.azure /home/*/.azure
rm -rf /tmp/* /var/tmp/*

# cloud-init state from the build boot. Cleaning it is what makes the next
# boot a first boot, so the buyer's user data is applied.
cloud-init clean --logs 2>/dev/null || rm -rf /var/lib/cloud/instances /var/lib/cloud/instance

find /var/log -type f -exec truncate -s 0 {} \;
truncate -s 0 /etc/machine-id   # systemd regenerates this per instance

# Anything the polign user wrote during the build is instance state, not image
# content: a shipped admin key would be a shipped credential.
rm -rf /var/lib/polign/* /var/cache/polign/* /var/lib/recall/*
rm -f /etc/recall/recall.env /etc/recall/*key*

history -c 2>/dev/null || true
sync
