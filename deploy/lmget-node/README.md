# Running an lmget node

A node mirrors the models of the signed lmget index and serves them by sha256 over HTTPS. Clients verify every byte against the signed index, so a node needs no trust, only bandwidth and disk.

This directory runs a node in a Proxmox container with no inbound port: `lmget-node serve` listens on loopback and a Cloudflare Tunnel publishes it. The container needs outbound DNS, outbound HTTPS (the model sources, the index and Cloudflare's apt repository) and outbound TCP 7844 (the tunnel); while `install.sh` runs, also the distribution mirrors its apt sources name; and, when seeding, outbound connections to any port.

## Files

| File | Role |
|---|---|
| `create-ct.sh` | Creates an unprivileged Debian 12 container (run on the Proxmox host). Refuses an existing VMID, and a static IP already configured on another container of the host. |
| `install.sh` | Installs `cloudflared` from Cloudflare's signed apt repository, creates the `lmget` and `cftunnel` system users and enables the units (run inside the container). Idempotent. |
| `deploy.sh` | From a workstation: builds `lmget-node` from a commit, with no uncommitted changes, for the container's architecture, copies it with the units and runs `install.sh`. Optionally writes the tunnel token. |
| `systemd/lmget-node.service` | `lmget-node serve` on `127.0.0.1:8080`, as `lmget`. Without a valid index it exits and retries every minute. |
| `systemd/lmget-node-mirror.{service,timer}` | `lmget-node mirror` two minutes after boot and every 6 hours: fetches and verifies the index, mirrors its models pinned to the signed commits. |
| `systemd/lmget-node-seed.service` | `lmget-node seed -port 6881` as `lmget`: every indexed blob as a BitTorrent v2 torrent on the DHT; re-reads the index every minute. Enabled with `SEED=1`. |
| `nftables-isolate.conf` | With `ISOLATE=1`: rules inside the container for a node on a flat LAN. Outbound: any port to IPv4 addresses outside `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`, `192.168.0.0/16`, `224.0.0.0/4` and `240.0.0.0/4` (private, CGNAT, loopback, link-local, multicast and reserved), which are refused, as are broadcast, multicast and all IPv6 beyond neighbor discovery on the link; DHCP from the client port to the router port is allowed. Inbound: replies to connections the node opened, DHCP replies and IPv6 neighbor discovery only; router adverts are ignored, so the node gets no global IPv6 address. |
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
- The units use `ProtectSystem=strict` and write only to `/var/lib/lmget-node`; check `systemctl status` on your host once.
