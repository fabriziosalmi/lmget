#!/bin/bash
# create-ct.sh: run on a Proxmox VE host. Creates an unprivileged Debian 12
# container for lmget-node. The container needs outbound DNS, outbound HTTPS
# (the sources, the index, Cloudflare's apt repository) and, for a Cloudflare
# Tunnel, outbound TCP 7844; while install.sh runs, also the distribution
# mirrors its apt sources name; when seeding, outbound connections to any
# port. Put it on a bridge that allows nothing else and accepts no inbound
# connection. Refuses an existing VMID, and a static IP already configured on
# another container of this host.
#
#   VMID=<id> IP=<address>/24 GW=<gateway> BRIDGE=<isolated bridge> ./create-ct.sh
#   VMID=<id> IP=dhcp BRIDGE=<LAN bridge> TEMPLATE=local:vztmpl/<template> ./create-ct.sh
set -euo pipefail
: "${VMID:?} ${IP:?} ${BRIDGE:?}"
STORAGE=${STORAGE:-local-lvm}
DISK=${DISK:-150}           # GB: the mirrored models live here
CORES=${CORES:-2}
MEMORY=${MEMORY:-2048}      # MB: lmget streams files, it does not hold them
TEMPLATE=${TEMPLATE:-local:vztmpl/debian-12-standard_12.12-1_amd64.tar.zst}
NAME=${NAME:-lmget-node}

if pct status "$VMID" >/dev/null 2>&1; then
	echo "VMID $VMID already exists: refusing" >&2
	exit 1
fi
if [ "$IP" = dhcp ]; then
	net="name=eth0,bridge=$BRIDGE,ip=dhcp,type=veth"
else
	: "${GW:?a static IP needs GW}"
	ip=${IP%/*}
	if pct list | awk 'NR>1{print $1}' | xargs -r -n1 pct config | grep -q "ip=$ip/"; then
		echo "$ip is already configured on another container: refusing" >&2
		exit 1
	fi
	net="name=eth0,bridge=$BRIDGE,ip=$IP,gw=$GW,type=veth"
fi
pct create "$VMID" "$TEMPLATE" \
	--hostname "$NAME" --unprivileged 1 --features keyctl=1 \
	--cores "$CORES" --memory "$MEMORY" --swap 512 \
	--rootfs "$STORAGE:$DISK" \
	--net0 "$net" \
	--nameserver "1.1.1.1 9.9.9.9" --onboot 1 --start 1
echo "created CT $VMID ($NAME) on $BRIDGE with $IP"
