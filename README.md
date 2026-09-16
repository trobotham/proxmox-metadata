# Proxmox Metadata Service

This service provides an AWS-style metadata endpoint for Proxmox guests at `169.254.169.254`. A privileged synchronization job queries the local Proxmox API with `pvesh`, reads VM addresses from `ipfilter-net0`, correlates IPv4 addresses with Kea subnets and gateways, and writes a sanitized configuration for an unprivileged HTTP service.

The binaries live in `/usr/local/sbin`, persistent configuration and documentation live in `/etc/proxmox-metadata`, and systemd units live in `/etc/systemd/system`. Only the generated configuration was moved from its original `/run/proxmox-metadata` location into `/etc/proxmox-metadata`.

## Architecture and files

```text
Proxmox API ─┐
             ├─ proxmox-metadata-sync ──> config.json ──> HTTP service
Kea config ──┘
```

```text
/usr/local/sbin/proxmox-metadata
/usr/local/sbin/proxmox-metadata-sync
/etc/proxmox-metadata/discovery.json       # edit this
/etc/proxmox-metadata/config.json          # generated; do not edit
/etc/proxmox-metadata/README.md

/etc/systemd/system/proxmox-metadata.service
/etc/systemd/system/proxmox-metadata-sync.service
/etc/systemd/system/proxmox-metadata-sync.timer
```

The sync timer runs as root because `pvesh` is privileged. The web server runs as an unprivileged user and cannot query or modify Proxmox.

## Security model

- IMDSv2-style tokens are required by default, expire after at most six hours, and are tied to the requester IP.
- VM identity is determined from the direct TCP source address. Forwarded-address headers are ignored.
- Do not put NAT, a reverse proxy, or a load balancer between guests and the service.
- Prefer exact `/32` and `/128` entries in `ipfilter-net0`.
- Map a `/64` only when it is exclusively routed to one VM.
- Never store passwords, private keys, API credentials, or other long-lived secrets in metadata.

## Build

```bash
apt update
apt install --yes golang-go jq curl
tar -xzf proxmox-metadata.tar.gz
cd proxmox-metadata
go build -trimpath -ldflags='-s -w' -o proxmox-metadata .
go build -trimpath -ldflags='-s -w' -o proxmox-metadata-sync ./cmd/proxmox-metadata-sync
```

## Continuous integration

`.github/workflows/ci.yml` runs on every push and pull request. It tests with Go 1.22 and the current stable Go release, runs the race detector, `go vet`, and ShellCheck, builds both binaries, and uploads the stable-Go binaries as a seven-day workflow artifact.

Run the same core checks locally:

```bash
go test -race -count=1 ./...
go vet ./...
shellcheck guest/proxmox-network-helper
mkdir -p build
go build -trimpath -o build/proxmox-metadata .
go build -trimpath -o build/proxmox-metadata-sync ./cmd/proxmox-metadata-sync
```

## Create the service account

This step is mandatory. If the user is missing, systemd fails with `status=217/USER`.

```bash
getent group proxmox-metadata >/dev/null || groupadd --system proxmox-metadata

getent passwd proxmox-metadata >/dev/null || \
  useradd --system \
    --gid proxmox-metadata \
    --home-dir /nonexistent \
    --shell /usr/sbin/nologin \
    proxmox-metadata

id proxmox-metadata
```

## Install

```bash
mkdir -p /etc/proxmox-metadata
install -d -o root -g proxmox-metadata -m 0750 /etc/proxmox-metadata
install -o root -g root -m 0755 ./proxmox-metadata /usr/local/sbin/proxmox-metadata
install -o root -g root -m 0755 ./proxmox-metadata-sync /usr/local/sbin/proxmox-metadata-sync
install -o root -g proxmox-metadata -m 0640 ./discovery.example.json /etc/proxmox-metadata/discovery.json
install -o root -g proxmox-metadata -m 0640 ./config.example.json /etc/proxmox-metadata/config.json
install -o root -g root -m 0644 ./README.md /etc/proxmox-metadata/README.md
install -o root -g root -m 0644 ./proxmox-metadata.service ./proxmox-metadata-sync.service ./proxmox-metadata-sync.timer /etc/systemd/system/
```

The example `config.json` is only a bootstrap file. The first successful synchronization replaces it entirely.

## Configure discovery

Edit `/etc/proxmox-metadata/discovery.json`:

```json
{
  "pvesh": "/usr/bin/pvesh",
  "kea_dhcp4_config": "/etc/kea/kea-dhcp4.conf",
  "output": "/etc/proxmox-metadata/config.json",
  "listen": "169.254.169.254:80",
  "require_token": true,
  "max_token_ttl_seconds": 21600,
  "ipv6_gateway": "2001:db8:100::2",
  "overrides": {}
}
```

The synchronizer obtains guest placement, type, lifecycle status, pool, tags, uptime, CPU and memory sizing, OS/architecture, firmware, machine type, safe guest settings, MAC addresses, bridges, VLANs, firewall flags, MTU, and rate limits. It excludes disks, password fields, API credentials, arbitrary QEMU arguments, and other likely secret-bearing values.

For each IPv4 address in `ipfilter-net0` or a tagged Kea reservation, it uses longest-prefix matching against Kea `subnet4` definitions and records the subnet ID, CIDR, `routers` gateway, `next-server`, and subnet options.

### Associate multiple Kea reservations with one VM

MAC addresses do not inherently identify a Proxmox VM, especially when secondary
addresses use synthetic or distinct DHCP MAC addresses. Add ownership metadata to
each Kea reservation with `user-context`. The context is ignored by DHCP clients
but consumed by `proxmox-metadata-sync`:

```json
{
  "subnet": "192.0.2.40/29",
  "id": 1,
  "option-data": [
    { "name": "routers", "data": "192.0.2.41" }
  ],
  "reservations": [
    {
      "hw-address": "02:00:00:00:00:43",
      "ip-address": "192.0.2.43",
      "user-context": {
        "proxmox-vmid": 101,
        "proxmox-primary": true
      }
    },
    {
      "hw-address": "02:00:00:00:00:44",
      "ip-address": "192.0.2.44",
      "user-context": {
        "proxmox-vmid": 101
      }
    }
  ]
}
```

Tag every reservation that belongs to the VM. Exactly one address should have
`"proxmox-primary": true`; all unmarked addresses are secondary. Reservations may
live in different Kea subnets and use different gateways or MAC addresses. VMID
may be a JSON number or numeric string. The equivalent nested form is also
accepted:

```json
"user-context": {
  "proxmox-metadata": {
    "vmid": 101,
    "primary": true
  }
}
```

The generated `/latest/meta-data/network/ipv4` records include `dhcp_mac` and
`primary`. The guest helper configures the primary first, then binds every
secondary address and installs a source-specific routing table using the gateway
from that address's Kea subnet. Tagged addresses are also authorized metadata
source addresses, so ownership tags must be treated as security-sensitive.

The synchronizer refuses to generate configuration when the same IPv4 address is
tagged for different VMIDs. If no address is explicitly primary, it retains the
legacy behavior and selects the lowest sorted IPv4 address.

## Special notes about overrides

Overrides are keyed by the numeric Proxmox VMID written as a JSON string:

```json
{
  "overrides": {
    "100": {
      "instance_id": "production-web-1",
      "hostname": "web-1",
      "ipv6_prefix": "2001:db8:100:1::/64",
      "custom_metadata": {
        "environment": "production",
        "role": "web-server",
        "owner": "infrastructure"
      },
      "user_data": "#cloud-config\nmanage_etc_hosts: true\n"
    }
  }
}
```

Important behavior:

- `instance_id`, `hostname`, and `ipv6_prefix` replace derived values.
- `custom_metadata` values must be strings and appear at `/latest/meta-data/custom/<key>`.
- `user_data` is returned verbatim at `/latest/user-data`; newlines must be JSON-escaped.
- An override does not create a VM mapping. The VM must exist and have either a usable `ipfilter-net0` or a tagged Kea reservation.
- Do not use overrides to repair missing source addresses; correct `ipfilter-net0` or the Kea reservation ownership tag instead.
- Never duplicate the same `/32`, `/128`, or equally specific source CIDR across VMs. Ambiguous requests are rejected.
- `/etc/proxmox-metadata/config.json` is regenerated every cycle. Manual edits are lost.
- Put only non-secret workload information in overrides.
- Validate and synchronize immediately after every change:

```bash
/usr/local/sbin/proxmox-metadata-sync -config /etc/proxmox-metadata/discovery.json -check
systemctl start proxmox-metadata-sync.service
```

## Configure the hypervisor network

Add these commands to the existing `vmbr0` IPv4 stanza; do not create a duplicate stanza:

```text
auto vmbr0
iface vmbr0 inet static
    address 192.0.2.42/29
    gateway 192.0.2.41
    ovs_type OVSBridge
    ovs_ports enp6s0f0

    post-up ip address replace 169.254.169.254/32 dev vmbr0
    post-up ip route replace 198.51.100.16/28 dev vmbr0 scope link

    pre-down ip route del 198.51.100.16/28 dev vmbr0 scope link || true
    pre-down ip address del 169.254.169.254/32 dev vmbr0 || true

iface vmbr0 inet6 static
    address 2001:db8:100::2/64
    gateway 2001:db8:100::1
```

Apply live without bringing down networking:

```bash
ip address replace 169.254.169.254/32 dev vmbr0
ip route replace 198.51.100.16/28 dev vmbr0 scope link
```

`192.0.2.40/29` is already connected through `192.0.2.42/29`. Every additional VM prefix on `vmbr0` needs an on-link host route, otherwise replies go to the default gateway instead of directly to the VM.

Verify both known blocks:

```bash
ip route get 192.0.2.46
ip route get 198.51.100.29
```

Both must show `dev vmbr0` without `via 192.0.2.41`.

## Configure Kea to distribute the guest route

Each VM needs `169.254.169.254/32` as an on-link route. Add DHCP option 121 inside every Kea subnet. Option 121 can supersede the ordinary router option, so include both the metadata route and default route.

For `192.0.2.40/29`:

```json
{
  "name": "classless-static-route",
  "data": "169.254.169.254/32 - 0.0.0.0, 0.0.0.0/0 - 192.0.2.41"
}
```

For `198.51.100.16/28`:

```json
{
  "name": "classless-static-route",
  "data": "169.254.169.254/32 - 0.0.0.0, 0.0.0.0/0 - 198.51.100.17"
}
```

```bash
kea-dhcp4 -t /etc/kea/kea-dhcp4.conf
systemctl restart kea-dhcp4-server
```

Renew the guest lease and confirm the route:

```bash
networkctl renew ens18
ip route show 169.254.169.254
```

Static-address guests do not consume Kea routes and must configure this route themselves.

## Start services

```bash
systemctl daemon-reload
systemctl enable --now proxmox-metadata-sync.timer
systemctl start proxmox-metadata-sync.service
systemctl enable --now proxmox-metadata.service
```

```bash
systemctl status proxmox-metadata-sync.service
systemctl status proxmox-metadata.service
systemctl list-timers proxmox-metadata-sync.timer
ss -lntp | grep '169.254.169.254:80'
```

The timer refreshes metadata approximately every minute and reloads the web service after successful synchronization.

## Query from a VM

```bash
curl -i http://169.254.169.254/healthz

TOKEN="$(curl -fsS -X PUT \
  -H 'X-aws-ec2-metadata-token-ttl-seconds: 3600' \
  http://169.254.169.254/latest/api/token)"

curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/identity-document | jq
```

Other useful endpoints:

```text
/latest/meta-data/
/latest/meta-data/instance-id
/latest/meta-data/vm-id
/latest/meta-data/local-hostname
/latest/meta-data/network/ipv4
/latest/meta-data/network/interfaces
/latest/meta-data/proxmox
/latest/meta-data/kea
/latest/meta-data/ipv6-prefix
/latest/user-data
```

## Guest network helper

The `guest/` directory contains a Linode-style network helper for Linux guests. It
retrieves `/latest/meta-data/network/ipv4`, binds every assigned IPv4 address, and
creates one source-policy routing table per address. This is important when a VM
has addresses from multiple prefixes with different gateways: traffic sourced
from each address leaves through the gateway associated with that prefix.

The guest must first have bootstrap connectivity to the metadata service, normally
from a DHCP lease containing the `169.254.169.254/32` classless static route. The
helper cannot discover assignments on a completely unconfigured interface.

On Ubuntu 22.04, install it inside the VM with:

```bash
sudo install -m 0755 guest/proxmox-network-helper /usr/local/sbin/proxmox-network-helper
sudo install -m 0644 guest/proxmox-network-helper.service /etc/systemd/system/
sudo install -m 0644 guest/proxmox-network-helper.timer /etc/systemd/system/
sudo install -m 0644 guest/proxmox-network-helper.default /etc/default/proxmox-network-helper
sudo systemctl daemon-reload
sudo systemctl enable --now proxmox-network-helper.timer
sudo systemctl start proxmox-network-helper.service
```

The interface is detected from the route to the metadata IP. If detection is not
appropriate for the guest, set `INTERFACE=ens18` in
`/etc/default/proxmox-network-helper`. The routing table and rule ranges can also
be overridden there; choose unused ranges if the guest already uses tables or
priorities beginning at `20100`.

Inspect the result with:

```bash
systemctl status proxmox-network-helper.service
journalctl -u proxmox-network-helper.service --no-pager
ip -4 address show dev ens18
ip -4 rule show
ip -4 route show table 20100
ip -4 route get 1.1.1.1 from 192.0.2.43
```

The helper only removes policy rules recorded in its own state file. It does not
flush the interface, replace the main default route, or delete addresses, which
avoids disconnecting the guest when metadata is temporarily unavailable. Run it
again at any time to pull newly assigned addresses and routes immediately:

```bash
sudo systemctl start proxmox-network-helper.service
```

## Troubleshooting

### `status=217/USER`

Create the `proxmox-metadata` group and service account as described above.

### VM has a route but cannot connect

```bash
# Proxmox
ip address show dev vmbr0
ss -lntp | grep ':80'
curl -i http://169.254.169.254/healthz
tcpdump -eni vmbr0 'arp or host 169.254.169.254'

# VM
ip route get 169.254.169.254
arping -I ens18 169.254.169.254
```

If Proxmox sends replies but the VM never receives them, run `ip route get <VM-IP>` on Proxmox. A result using the default gateway is wrong; install an on-link route for that VM prefix.

### Token endpoint returns `403`

The source address was not uniquely associated with a VM:

```bash
journalctl -u proxmox-metadata -n 30 --no-pager

jq --arg ip "198.51.100.29/32" '
  [.instances[]
   | select(any(.sources[]?; . == $ip))
   | {vm_id, hostname, sources}]
' /etc/proxmox-metadata/config.json

pvesh get /nodes/<NODE>/qemu/<VMID>/firewall/ipset/ipfilter-net0 \
  --output-format json-pretty
```

Correct `ipfilter-net0`, synchronize, and retry.

### Synchronizer cannot write `config.json`

The sync unit must contain:

```text
ReadWritePaths=/etc/proxmox-metadata
```

This permits the write despite `ProtectSystem=strict`.

### Full validation

```bash
/usr/local/sbin/proxmox-metadata-sync -config /etc/proxmox-metadata/discovery.json -check
/usr/local/sbin/proxmox-metadata -config /etc/proxmox-metadata/config.json -check
systemctl restart proxmox-metadata-sync.service
systemctl restart proxmox-metadata.service
journalctl -u proxmox-metadata-sync -u proxmox-metadata -n 100 --no-pager
```
