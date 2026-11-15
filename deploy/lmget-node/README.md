# Running an lmget node

A node mirrors the models of the signed lmget index and serves them by sha256 over HTTPS. Clients verify every byte against the signed index, so a node needs no trust, only bandwidth and disk.

This directory runs a node in a Proxmox container with no inbound port: `lmget-node serve` listens on loopback and a Cloudflare Tunnel publishes it. The container needs outbound HTTPS (the model sources and the index) and outbound TCP 7844 (the tunnel), nothing else.

## Files

| File | Role |
|---|---|
| `create-ct.sh` | Creates an unprivileged Debian 12 container (run on the Proxmox host). Refuses an existing VMID or IP. |
| `install.sh` | Installs `cloudflared` from Cloudflare's signed apt repository, creates the `lmget` and `cftunnel` system users and enables the units (run inside the container). Idempotent. |
| `deploy.sh` | From a workstation: builds `lmget-node` from a clean commit for the container's architecture, copies it with the units and runs `install.sh`. Optionally writes the tunnel token. |
| `systemd/lmget-node.service` | `lmget-node serve` on `127.0.0.1:8080`, as `lmget`. Without a valid index it exits and retries every minute. |
| `systemd/lmget-node-mirror.{service,timer}` | `lmget-node mirror` two minutes after boot and every 6 hours: fetches and verifies the index, mirrors its models pinned to the signed commits. |
| `systemd/lmget-tunnel.service` | `cloudflared tunnel run` (http2) as `cftunnel`, with `TUNNEL_TOKEN` read from `/etc/lmget-node/tunnel.env` (root only, never on a command line). Skipped until that file exists. |

## Steps

1. Create the tunnel in the Cloudflare dashboard (Zero Trust, Networks, Tunnels), with a public hostname such as `node1.example.org` pointing to `http://127.0.0.1:8080`, and keep its token in your secrets manager.
2. On the Proxmox host: `VMID=<id> IP=<address>/24 GW=<gateway> BRIDGE=vmbr1 ./create-ct.sh`.
3. From the workstation: `PVE=root@<pve> VMID=<id> TOKEN_CMD='<command that prints the token>' deploy/lmget-node/deploy.sh`.
4. Check from outside, not from the container: `curl -sI https://node1.example.org/index.json` answers 200 once the first mirror has run.
5. To be listed in the index, the node's URL goes into the index the maintainer builds and signs (`lmget-index build -node https://node1.example.org ...`).

## Notes

- `lmget-node seed` (BitTorrent v2) is not enabled here: it needs inbound and outbound ports that a tunnel-only container does not have.
- Disk: size the container for the models of the index. A model is mirrored whole, at the commit the index signs.
- The units use `ProtectSystem=strict` and write only to `/var/lib/lmget-node`; they were tested in a Debian 12 system container (OrbStack); check `systemctl status` on your host once.
