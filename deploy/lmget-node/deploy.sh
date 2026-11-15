#!/bin/bash
# deploy.sh: from a workstation, build lmget-node from a clean commit and
# install it in a Proxmox container created by create-ct.sh.
#
#   PVE=root@<proxmox host> VMID=<id> deploy/lmget-node/deploy.sh
#
# With TOKEN_CMD set, the Cloudflare Tunnel token is read from that command's
# stdout and written to /etc/lmget-node/tunnel.env (0600) through stdin: it
# never appears on a command line or in this script's output, e.g.
#   TOKEN_CMD='infisical secrets get LMGET_NODE1_TUNNEL_TOKEN --env prod --plain --silent --projectId ...'
set -euo pipefail
: "${PVE:?} ${VMID:?}"
REPO=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$REPO"
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "the working tree has uncommitted changes: deploy a commit" >&2
	exit 1
fi
rev=$(git rev-parse --short HEAD)
ARCH=$(ssh "$PVE" "pct exec $VMID -- dpkg --print-architecture")
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
echo "== build lmget-node $rev for linux/$ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH GOFLAGS=-tags=purego_utp \
	go build -trimpath -ldflags "-s -w -buildid= -X github.com/fabriziosalmi/lmget.Version=$rev" \
	-o "$stage/lmget-node" ./cmd/lmget-node
cp -R deploy/lmget-node/install.sh deploy/lmget-node/systemd "$stage/"
shasum -a 256 "$stage/lmget-node"

echo "== copy to CT $VMID and install"
tar -C "$stage" -cf - . | ssh "$PVE" "rm -rf /tmp/lmget-node-deploy && mkdir /tmp/lmget-node-deploy &&
	tar -C /tmp/lmget-node-deploy -xf - && cd /tmp/lmget-node-deploy &&
	pct exec $VMID -- rm -rf /root/lmget-node-deploy &&
	pct exec $VMID -- mkdir -p /root/lmget-node-deploy/systemd &&
	for f in lmget-node install.sh systemd/*; do pct push $VMID /tmp/lmget-node-deploy/\$f /root/lmget-node-deploy/\$f; done &&
	cd / && rm -rf /tmp/lmget-node-deploy"
if [ -n "${TOKEN_CMD:-}" ]; then
	echo "== tunnel token -> /etc/lmget-node/tunnel.env (0600, through stdin)"
	eval "$TOKEN_CMD" | tr -d '\r\n' | ssh "$PVE" "pct exec $VMID -- sh -c 'umask 077; mkdir -p /etc/lmget-node; { printf TUNNEL_TOKEN=; cat; echo; } > /etc/lmget-node/tunnel.env'"
fi
ssh "$PVE" "pct exec $VMID -- bash /root/lmget-node-deploy/install.sh"
echo "== deployed $rev to CT $VMID"
