#!/bin/bash
# Installs the pinned Recall and polign_db releases, the embedding model, and
# the systemd units.
#
# Every download happens here, at build time, and is checked against a pinned
# checksum. Nothing is downloaded at boot: a Marketplace image has to come up
# without reaching the internet.
set -euo pipefail

: "${RECALL_VERSION:?}"
: "${POLIGN_VERSION:?}"
: "${ARCH:?}"

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get -y upgrade
apt-get -y install curl ca-certificates tar gzip openssl

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"

# fetch <release base url> <asset>: downloads one release asset and checks it
# against that release's checksums.txt.
fetch() {
  curl -fsSL --retry 5 --retry-delay 2 -o "$2" "$1/$2"
  curl -fsSL --retry 5 --retry-delay 2 -o checksums.txt "$1/checksums.txt"
  grep " $2\$" checksums.txt | sha256sum -c -
}

# polign_db: the backend. Its archive is flat, binaries at the root.
fetch "https://github.com/Polign/polign/releases/download/${POLIGN_VERSION}" "polign_db_linux_${ARCH}.tar.gz"
mkdir -p "/opt/polign/${POLIGN_VERSION}"
tar -xzf "polign_db_linux_${ARCH}.tar.gz" -C "/opt/polign/${POLIGN_VERSION}"
for b in polign polign-server polign-maintain polign-apikey polign-import; do
  [ -f "/opt/polign/${POLIGN_VERSION}/$b" ] && install -m 0755 "/opt/polign/${POLIGN_VERSION}/$b" "/usr/local/bin/$b"
done
test "$(/usr/local/bin/polign-server -version)" = "${POLIGN_VERSION#v}"

# Recall: the memory server in front of it.
fetch "https://github.com/Polign/recall/releases/download/${RECALL_VERSION}" "recall_linux_${ARCH}.tar.gz"
mkdir -p "/opt/recall/${RECALL_VERSION}"
tar -xzf "recall_linux_${ARCH}.tar.gz" -C "/opt/recall/${RECALL_VERSION}"
install -m 0755 "/opt/recall/${RECALL_VERSION}/recall" /usr/local/bin/recall
/usr/local/bin/recall --version | grep -q "${RECALL_VERSION#v}"

# The built-in embedding model, so search by meaning works on first boot with
# no API key and no internet access. Same revision and hashes as model/local.go;
# change both together.
MODEL_DIR=/opt/recall/models/all-minilm-l6-v2
HF=https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/751bff37182d3f1213fa05d7196b954e230abad9
mkdir -p "$MODEL_DIR"
while read -r path sum; do
  curl -fsSL --retry 5 --retry-delay 2 -o "$MODEL_DIR/$(basename "$path")" "$HF/$path"
  echo "$sum  $MODEL_DIR/$(basename "$path")" | sha256sum -c -
done <<'FILES'
onnx/model.onnx 759c3cd2b7fe7e93933ad23c4c9181b7396442a2ed746ec7c1d46192c469c46e
tokenizer.json da0e79933b9ed51798a3ae27893d3c5fa4a201126cef75586296df9b4d2c62a0
tokenizer_config.json 9261e7d79b44c8195c1cada2b453e55b00aeb81e907a6664974b4d7776172ab3
special_tokens_map.json b6d346be366a7d1d48332dbc9fdf3bf8960b5d879522b7799ddba59e76237ee3
config.json 7135149f7cffa1a573466c6e4d8423ed73b62fd2332c575bf738a0d033f70df7
FILES
chmod -R a+rX /opt/recall/models

# Two service accounts: polign runs the database, recall runs the server in
# front of it. Neither has a login shell or a password.
useradd --system --home-dir /var/lib/polign --shell /usr/sbin/nologin polign 2>/dev/null || true
useradd --system --home-dir /var/lib/recall --shell /usr/sbin/nologin recall 2>/dev/null || true
install -d -o root -g root -m 0755 /etc/recall
install -d -o polign -g polign -m 0750 /var/lib/polign /var/cache/polign
install -d -o recall -g recall -m 0750 /var/lib/recall

install -m 0755 /tmp/recall-files/recall-firstboot /usr/local/sbin/recall-firstboot
install -m 0644 /tmp/recall-files/recall-firstboot.service /etc/systemd/system/recall-firstboot.service
install -m 0644 /tmp/recall-files/polign-server.service /etc/systemd/system/polign-server.service
install -m 0644 /tmp/recall-files/recall.service /etc/systemd/system/recall.service
install -m 0644 /tmp/recall-files/recall.env.defaults /etc/recall/recall.env.defaults

systemctl daemon-reload
systemctl enable recall-firstboot.service polign-server.service recall.service
rm -rf /tmp/recall-files
