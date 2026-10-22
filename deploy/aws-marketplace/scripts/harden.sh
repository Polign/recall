#!/bin/bash
# Marketplace AMI policy: no password logins, no root SSH, no leftover
# credentials, and an agent the buyer can reach the instance with.
set -euo pipefail

mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/60-recall.conf <<'CONF'
PasswordAuthentication no
PermitRootLogin no
PermitEmptyPasswords no
ChallengeResponseAuthentication no
CONF

# Lock every password-capable account. AL2023 ships none set, this keeps a
# base-image change from turning into a shipped credential.
passwd -l root >/dev/null 2>&1 || true
awk -F: '($2 != "*" && $2 != "!!" && $2 !~ /^!/) {print $1}' /etc/shadow | while read -r u; do
  passwd -l "$u" >/dev/null 2>&1 || true
done

# SSM keeps day-2 access working without an inbound port.
dnf -y install amazon-ssm-agent 2>/dev/null || true
systemctl enable amazon-ssm-agent 2>/dev/null || true

systemctl enable chronyd 2>/dev/null || true
