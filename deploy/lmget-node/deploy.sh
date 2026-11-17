#!/bin/bash
# deploy.sh: from a workstation, build lmget-node from a clean commit and
# install it in a Proxmox container created by create-ct.sh.
#
#   PVE=root@<proxmox host> VMID=<id> deploy/lmget-node/deploy.sh
#   PVE=root@<proxmox host> VMID=<id> SEED=1 ISOLATE=1 deploy/lmget-node/deploy.sh
#
# With TOKEN_CMD set, the Cloudflare Tunnel token is read from that command's
# stdout and written to /etc/lmget-node/tunnel.env (0600) through stdin: it
# never appears on a command line or in this script's output, e.g.
#   TOKEN_CMD='<your secrets manager command that prints the token>'
set -euo pipefail
: "${PVE:?} ${VMID:?}"
REPO=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$REPO"
# untracked files count too: a stray .go file in a built package is compiled in
if [ -n "$(git status --porcelain)" ]; then
	echo "the working tree has uncommitted or untracked changes: deploy a commit" >&2
	exit 1
fi
export GOWORK=off # a go.work in a parent directory must not change what is built
rev=$(git rev-parse --short HEAD)
ARCH=$(ssh "$PVE" "pct exec $VMID -- dpkg --print-architecture")
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
echo "== build lmget-node $rev for linux/$ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH GOFLAGS=-tags=purego_utp \
	go build -trimpath -ldflags "-s -w -buildid= -X github.com/fabriziosalmi/lmget.Version=$rev" \
	-o "$stage/lmget-node" ./cmd/lmget-node
cp -R deploy/lmget-node/install.sh deploy/lmget-node/nftables-isolate.conf deploy/lmget-node/systemd "$stage/"
shasum -a 256 "$stage/lmget-node"

echo "== copy to CT $VMID and install"
tar -C "$stage" -cf - . | ssh "$PVE" "rm -rf /tmp/lmget-node-deploy && mkdir /tmp/lmget-node-deploy &&
	tar -C /tmp/lmget-node-deploy -xf - && cd /tmp/lmget-node-deploy &&
	pct exec $VMID -- rm -rf /root/lmget-node-deploy &&
	pct exec $VMID -- mkdir -p /root/lmget-node-deploy/systemd &&
	for f in lmget-node install.sh nftables-isolate.conf systemd/*; do pct push $VMID /tmp/lmget-node-deploy/\$f /root/lmget-node-deploy/\$f; done &&
	cd / && rm -rf /tmp/lmget-node-deploy"
if [ -n "${TOKEN_CMD:-}" ]; then
	echo "== tunnel token -> /etc/lmget-node/tunnel.env (0600, through stdin)"
	token=$(eval "$TOKEN_CMD" | tr -d '\r\n')
	[ -n "$token" ] || { echo "TOKEN_CMD gave no token: tunnel.env left as it was" >&2; exit 1; }
	# written to a temporary file and renamed: a failed write never leaves an empty token behind
	printf '%s' "$token" | ssh "$PVE" "pct exec $VMID -- sh -c 'umask 077; mkdir -p /etc/lmget-node && { printf TUNNEL_TOKEN=; cat; echo; } > /etc/lmget-node/tunnel.env.new && mv /etc/lmget-node/tunnel.env.new /etc/lmget-node/tunnel.env'"
	unset token
fi
ssh "$PVE" "pct exec $VMID -- env SEED=${SEED:-0} ISOLATE=${ISOLATE:-0} bash /root/lmget-node-deploy/install.sh"
echo "== deployed $rev to CT $VMID"
