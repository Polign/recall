#!/bin/bash
# Azure Marketplace VM image policy: no password logins, no root SSH, no
# leftover credentials, the Azure Linux Agent left running, no swap on the OS
# disk, and a way for the buyer to reach the instance.
set -euo pipefail

mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/60-recall.conf <<'CONF'
PasswordAuthentication no
PermitRootLogin no
PermitEmptyPasswords no
KbdInteractiveAuthentication no
# Azure certification asks for a keepalive so idle sessions survive the
# platform's connection timeout.
ClientAliveInterval 180
CONF

# Lock every password-capable account. The Debian image ships none set, and
# this keeps a base-image change from turning into a shipped credential.
passwd -l root >/dev/null 2>&1 || true
awk -F: '($2 != "*" && $2 != "!" && $2 !~ /^!/) {print $1}' /etc/shadow | while read -r u; do
  passwd -l "$u" >/dev/null 2>&1 || true
done

# The Azure Linux Agent (waagent) is what makes provisioning, SSH key
# injection, extensions, and the serial console work, and cloud-init is what
# applies the buyer's user data. Both are preinstalled on the Debian image;
# keep them enabled.
systemctl enable walinuxagent 2>/dev/null || systemctl enable waagent 2>/dev/null || true
systemctl enable cloud-init 2>/dev/null || true

# Certification rejects images that swap on the OS disk. The temporary
# resource disk is the sanctioned place, and the default is off; pin it.
if [ -f /etc/waagent.conf ]; then
  sed -i 's/^ResourceDisk.EnableSwap=.*/ResourceDisk.EnableSwap=n/' /etc/waagent.conf
  sed -i 's/^Provisioning.RegenerateSshHostKeyPair=.*/Provisioning.RegenerateSshHostKeyPair=y/' /etc/waagent.conf
fi

systemctl enable systemd-timesyncd 2>/dev/null || true

# Unattended security updates. A long-lived database VM that never patches is
# a liability, and Marketplace reviewers look for this.
export DEBIAN_FRONTEND=noninteractive
apt-get -y install unattended-upgrades 2>/dev/null || true
systemctl enable unattended-upgrades 2>/dev/null || true
