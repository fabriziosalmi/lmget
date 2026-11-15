#!/bin/bash
# install.sh: run as root inside the node's container (Debian 12), from the
# directory that holds lmget-node, this script and systemd/. Idempotent.
# The tunnel token is not handled here: /etc/lmget-node/tunnel.env is written
# separately (see deploy.sh) and the tunnel starts only once it exists.
set -euo pipefail
cd "$(dirname "$0")"

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q ca-certificates curl gpg

# cloudflared from Cloudflare's signed apt repository
if ! command -v cloudflared >/dev/null; then
	install -d -m 0755 /usr/share/keyrings
	curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
	echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' \
		>/etc/apt/sources.list.d/cloudflared.list
	apt-get update -q
	apt-get install -y -q cloudflared
fi

# users: lmget owns the store, cftunnel runs the tunnel; neither can log in
id lmget >/dev/null 2>&1 || useradd --system --home-dir /var/lib/lmget-node --shell /usr/sbin/nologin lmget
id cftunnel >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin cftunnel
install -d -o lmget -g lmget -m 0750 /var/lib/lmget-node
install -d -o root -g root -m 0700 /etc/lmget-node

# no shell access over the network: the operator uses pct exec. The node
# listens only on loopback (lmget-node 8080, the local MTA)
systemctl disable --now ssh.socket ssh.service >/dev/null 2>&1 || true

install -m 0755 lmget-node /usr/local/bin/lmget-node
install -m 0644 systemd/*.service systemd/*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now lmget-node-mirror.timer
systemctl enable lmget-node.service lmget-tunnel.service
systemctl restart lmget-node.service
if [ -s /etc/lmget-node/tunnel.env ]; then
	systemctl restart lmget-tunnel.service
else
	echo "no /etc/lmget-node/tunnel.env yet: the tunnel starts once it is written"
fi
sha256sum /usr/local/bin/lmget-node
