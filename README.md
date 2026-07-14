# telegraf-input-netconf

An [`execd`](https://github.com/influxdata/telegraf/tree/master/plugins/inputs/execd)
input plugin for [Telegraf](https://github.com/influxdata/telegraf) that collects
interface counters from NETCONF-enabled network devices (Cisco, Juniper, …) over
SSH.

SSH sessions are opened once and kept alive across polls to avoid paying the
session-setup cost on every gather cycle. A session that fails an RPC is dropped
and transparently re-established on the next cycle.

> **Status: beta.** This has only been exercised in a lab and not yet validated
> against production Cisco or Juniper hardware. The interface-statistics data
> model in particular (see [Caveats](#caveats)) is likely to need per-platform
> adjustment. Use it in a lab first.

## What it collects

For each configured device it issues a NETCONF `<get>` for `ietf-interfaces`
interface statistics and emits one measurement per interface:

- **Measurement:** `netconf_interface`
- **Tags:** `device` (the configured address), `interface` (interface name)
- **Fields:** `input_bytes`, `output_bytes` (from `in-octets` / `out-octets`)

## Build

Requires Go (see `go.mod` for the minimum version).

```sh
go build -o bin/netconf-client ./cmd/netconf-client
```

## Configuration

The binary is a Telegraf `execd` shim. Point it at a plugin config file:

```sh
bin/netconf-client --config cmd/netconf-client/plugin.conf
```

Example `plugin.conf`:

```toml
[[inputs.netconf]]
  ## Timeout for connecting to and polling each device.
  # timeout = "10s"

  ## Path to an OpenSSH known_hosts file used to verify device host keys.
  ## Defaults to ~/.ssh/known_hosts.
  # known_hosts_file = "/etc/telegraf/known_hosts"

  ## Disable host key verification entirely. INSECURE - lab use only.
  # insecure_skip_verify = false

  [[inputs.netconf.devices]]
    address = "192.168.1.1:830"
    username = "admin"
    ## Password supports Telegraf secret-store references, e.g. "@{store:key}".
    password = "password"

  [[inputs.netconf.devices]]
    address = "192.168.1.2:830"
    username = "admin"
    password = "password"
```

Then reference the shim from your main Telegraf config with the
[`execd`](https://github.com/influxdata/telegraf/tree/master/plugins/inputs/execd)
input:

```toml
[[inputs.execd]]
  command = ["/path/to/netconf-client", "--config", "/path/to/plugin.conf"]
  signal = "none"
```

### Host key verification

By default the plugin verifies each device's SSH host key against an OpenSSH
`known_hosts` file (`~/.ssh/known_hosts` unless `known_hosts_file` is set). If
the file cannot be loaded, `Init` fails with a clear error. Set
`insecure_skip_verify = true` to bypass verification — this disables a real
security control and is intended for lab use only.

### Passwords

`password` is a Telegraf [secret](https://github.com/influxdata/telegraf/blob/master/docs/CONFIGURATION.md#secret-store-secrets),
so it can reference a configured secret store (e.g. `"@{store:device_pw}"`)
instead of being written in plaintext.

## Caveats

- **Data model.** `ietf-interfaces` exposes operational counters under
  `/interfaces-state/interface/statistics` on RFC 7223 devices and under
  `/interfaces/interface/statistics` on RFC 8343 (NMDA) devices. The current
  request/parse path targets the latter and may need adjusting for your
  platform.
- Only interface `in-octets` / `out-octets` are collected today.

## License

GNU General Public License v3.0 or later. See [LICENSE](LICENSE).
