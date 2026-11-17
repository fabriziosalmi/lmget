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
| `systemd/lmget-node-seed.service` | `lmget-node seed -port 6881` as `lmget`: every indexed blob as a BitTorrent v2 torrent on the DHT; re-reads the index every minute. Enabled with `SEED=1`. |
| `nftables-isolate.conf` | With `ISOLATE=1`: rules inside the container for a node on a flat LAN. Outbound: any port to public IPv4 addresses; private and special IPv4 ranges, broadcast, multicast and all IPv6 beyond neighbor discovery on the link are refused, except DHCP from the client port to the router port. Inbound: only replies to connections the node opened; router adverts are ignored, so the node gets no global IPv6 address. |
| `systemd/lmget-tunnel.service` | `cloudflared tunnel run` (http2) as `cftunnel`, with `TUNNEL_TOKEN` read from `/etc/lmget-node/tunnel.env` (root only, never on a command line). Skipped until that file exists. |

## Steps

1. Create the tunnel in the Cloudflare dashboard (Zero Trust, Networks, Tunnels), with a public hostname such as `node1.example.org` pointing to `http://127.0.0.1:8080`, and keep its token in your secrets manager.
2. On the Proxmox host: `VMID=<id> IP=<address>/24 GW=<gateway> BRIDGE=<bridge> ./create-ct.sh` (or `IP=dhcp`, and `TEMPLATE=` for another template).
3. From the workstation: `PVE=root@<pve> VMID=<id> TOKEN_CMD='<command that prints the token>' deploy/lmget-node/deploy.sh` (add `SEED=1 ISOLATE=1` for a node on a flat LAN, and keep them on every later deploy).
4. Check from outside, not from the container: `curl -sI https://node1.example.org/index.json` answers 200 once the first mirror has run.
5. To be listed in the index, the node's URL goes into the index the maintainer builds and signs (`lmget-index build -node https://node1.example.org ...`).

## Notes

- `lmget-node seed` needs outbound ports beyond 443: enable it (`SEED=1`) only where the network allows them. Without an inbound port it reaches the peers it connects to, not the other way round.
- On a flat LAN with no isolated bridge, `ISOLATE=1` keeps the node away from the other hosts without changing the Proxmox host or the router. Check it from inside the container against a LAN address that answers without the rules.
- Disk: size the container for the models of the index. A model is mirrored whole, at the commit the index signs.
- The units use `ProtectSystem=strict` and write only to `/var/lib/lmget-node`; they were tested in a Debian 12 system container (OrbStack); check `systemctl status` on your host once.
