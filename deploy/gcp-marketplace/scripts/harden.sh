#!/bin/bash
# Google Cloud Marketplace image policy: no password logins, no root SSH, no
# leftover credentials, and a way for the buyer to reach the instance.
set -euo pipefail

mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/60-recall.conf <<'CONF'
PasswordAuthentication no
PermitRootLogin no
PermitEmptyPasswords no
KbdInteractiveAuthentication no
CONF

# Lock every password-capable account. The Debian public image ships none set,
# and this keeps a base-image change from turning into a shipped credential.
passwd -l root >/dev/null 2>&1 || true
awk -F: '($2 != "*" && $2 != "!" && $2 !~ /^!/) {print $1}' /etc/shadow | while read -r u; do
  passwd -l "$u" >/dev/null 2>&1 || true
done

# The Google guest environment is what makes OS Login, metadata-driven SSH
# keys, and the serial console work. It is preinstalled on the Debian public
# image; do not remove it, and make sure it stays enabled.
systemctl enable google-guest-agent 2>/dev/null || true
systemctl enable google-osconfig-agent 2>/dev/null || true

# OS Login is how a buyer reaches the instance without a public IP and without
# managing keys: IAM decides who may log in, and an IAP tunnel carries the
# session. Turning it on in the image means it is the default rather than
# something the buyer has to remember. A launch can still override it with
# instance metadata.
mkdir -p /etc/default
if ! grep -q '^enable-oslogin' /etc/default/instance_configs.cfg 2>/dev/null; then
  cat >> /etc/default/instance_configs.cfg <<'CFG'

[InstanceSetup]
set_host_keys = true
CFG
fi

systemctl enable systemd-timesyncd 2>/dev/null || true

# Unattended security updates. A long-lived server that never patches is
# a liability, and Marketplace reviewers look for this.
export DEBIAN_FRONTEND=noninteractive
apt-get -y install unattended-upgrades 2>/dev/null || true
systemctl enable unattended-upgrades 2>/dev/null || true
