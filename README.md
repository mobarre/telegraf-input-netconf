# telegraf-input-netconf

An [`execd`](https://github.com/influxdata/telegraf/tree/master/plugins/inputs/execd)
input plugin for [Telegraf](https://github.com/influxdata/telegraf) that collects
metrics from NETCONF-over-SSH devices (Cisco, Juniper, …).

It is **model-agnostic**: what to poll and how to turn the reply into metrics is
entirely user-configured through *sensors* — a NETCONF RPC plus a Telegraf
[XPath](https://github.com/influxdata/telegraf/tree/master/plugins/parsers/xpath)
mapping. Interfaces, BGP, system state, or any YANG data are just different
sensors; the plugin core is not tied to any data model.

It runs as a **single daemon holding one warm SSH/NETCONF session per device**.
The session handshake is paid once per device, not once per poll; sessions are
reused across cycles, recycled after a long timer (`session_max_age`) or on
failure, and devices are polled concurrently. The reasoning behind this design —
and the alternatives that were rejected — is documented in [DESIGN.md](DESIGN.md).

> **Status: beta.** Validated against Cisco IOS-XR (NCS5500 and NCS5700) in a
> staging network; not yet exercised against Juniper or at production scale.
> The example sensor's data model is platform-specific (see
> [Caveats](#caveats)).

## Metrics

There are no built-in measurements — each sensor defines its own via its XPath
mapping. Every metric is additionally tagged with `device` (the configured
address). The example sensor below produces:

- **Measurement:** `netconf_interface`
- **Tags:** `device`, `interface`
- **Fields:** `in_octets`, `out_octets`, `in_pkts`, `out_pkts`

## Build

Requires Go (see `go.mod` for the minimum version).

```sh
go build -o bin/netconf-client ./cmd/netconf-client
```

## Running

The binary is a Telegraf `execd` shim. Reference it from your main Telegraf
config, letting our daemon drive its own poll cadence:

```toml
[[inputs.execd]]
  command = ["/path/to/netconf-client", "--config", "/etc/telegraf/netconf/plugin.conf", "--poll_interval", "5m"]
  ## The daemon self-times on --poll_interval; the parent only consumes stdout.
  signal = "none"
```

## Configuration

Example `plugin.conf` (one instance owns the whole fleet):

```toml
[[inputs.netconf]]
  ## Timeout for connecting to and polling each device.
  # timeout = "10s"

  ## How long a session is reused before it is proactively recycled.
  # session_max_age = "1h"

  ## Maximum number of devices dialed/polled concurrently (0 = unlimited).
  ## Matters most at start-up and recycle, where it bounds the dial burst.
  # max_concurrent_devices = 10

  ## Path to an OpenSSH known_hosts file used to verify device host keys.
  ## Required unless insecure_skip_verify is set (see below).
  known_hosts_file = "/etc/telegraf/known_hosts"

  ## Disable host key verification entirely. INSECURE - lab use only.
  # insecure_skip_verify = false

  ## Devices may be listed inline and/or loaded from a directory (see below).
  # devices_directory = "/etc/telegraf/netconf.d"

  [[inputs.netconf.devices]]
    address = "192.168.1.1:830"
    username = "admin"
    ## Password supports Telegraf secret-store references, e.g. "@{store:key}".
    password = "password"

  ## One or more sensors. Tip: match elements with local-name() to ignore
  ## NETCONF's XML namespaces without declaring them.
  [[inputs.netconf.sensor]]
    rpc = '''
      <get>
        <filter type="subtree">
          <interfaces xmlns="http://openconfig.net/yang/interfaces"/>
        </filter>
      </get>
    '''
    metric_name = "'netconf_interface'"
    metric_selection = "//*[local-name()='interface']"
    [inputs.netconf.sensor.tags]
      interface = "*[local-name()='name']"
    ## More counters live under state/counters (errors, discards, unicast/
    ## broadcast/multicast pkts, …); add the ones you need.
    [inputs.netconf.sensor.fields_int]
      in_octets  = "*[local-name()='state']/*[local-name()='counters']/*[local-name()='in-octets']"
      out_octets = "*[local-name()='state']/*[local-name()='counters']/*[local-name()='out-octets']"
      in_pkts    = "*[local-name()='state']/*[local-name()='counters']/*[local-name()='in-pkts']"
      out_pkts   = "*[local-name()='state']/*[local-name()='counters']/*[local-name()='out-pkts']"
```

### Sensors

A sensor's `rpc` is sent verbatim (wrapped in `<rpc>…</rpc>`), and the reply is
handed to a Telegraf XPath parser. All
[xpath parser options](https://github.com/influxdata/telegraf/tree/master/plugins/parsers/xpath)
— `metric_name`, `metric_selection`, `tags`, `fields`, `fields_int`,
`timestamp`, etc. — are available as keys of the `[[inputs.netconf.sensor]]`
table. Add one sensor per thing you want to poll.

### Devices directory

For large fleets, keep one file per device (easy to template and manage) and
point `devices_directory` at them. Each `*.toml` file holds one or more
`[[devices]]` entries, merged into the single instance at start-up:

```toml
# /etc/telegraf/netconf.d/r1.toml
[[devices]]
  address = "r1:830"
  username = "admin"
  password = "@{store:r1}"
```

Inline `[[inputs.netconf.devices]]` and `devices_directory` are both honored and
merged. Add or remove a device by dropping/deleting a file and reloading
Telegraf.

### Host key verification

The plugin verifies each device's SSH host key against an OpenSSH `known_hosts`
file. `known_hosts_file` is **required** — there is no default, because the
telegraf user often has no home directory. If the file cannot be loaded, `Init`
fails with a clear error. Set `insecure_skip_verify = true` to bypass
verification entirely — this disables a real security control and is intended
for lab use only.

### Passwords

`password` is a Telegraf [secret](https://github.com/influxdata/telegraf/blob/master/docs/CONFIGURATION.md#secret-store-secrets),
so it can reference a configured secret store (e.g. `"@{store:device_pw}"`)
instead of being written in plaintext.

## Caveats

- **Example data model.** The example sensor uses OpenConfig
  (`http://openconfig.net/yang/interfaces`), whose counters live under
  `/interfaces/interface/state/counters`. This was chosen because on Cisco
  IOS-XR the `ietf-interfaces` model is config-only (a plain `<get>` returns no
  operational counters). Other platforms may expose a different model or
  namespace; adjust the sensor's filter and XPaths accordingly.
- **Field-less metrics are dropped.** If a `metric_selection` matches nodes that
  carry none of the configured fields (e.g. SRv6 locator "interfaces" have no
  counters), the resulting tag-only metric is discarded rather than emitted —
  an empty metric is unusable and cannot be serialized.
- Secret-store references (`@{store:…}`) depend on the secret-store machinery
  being available in the runtime.

## License

GNU General Public License v3.0 or later. See [LICENSE](LICENSE).
