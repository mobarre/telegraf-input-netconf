# Design notes

This document explains *why* the plugin is built the way it is. The code tells you what it does; this tells you the reasoning, the constraints that forced each choice, and the tempting-but-wrong alternatives we deliberately rejected. If you are reading this months later wondering "why on earth did I do it this way", start here.

## What this plugin is

It is a Telegraf [`execd`](https://github.com/influxdata/telegraf/tree/master/plugins/inputs/execd) input plugin that collects metrics from NETCONF-over-SSH devices (Cisco, Juniper, …).

It is **model-agnostic**: what to poll and how to turn the reply into metrics is entirely user-configured via *sensors* (a NETCONF RPC payload + a Telegraf XPath mapping). The plugin core knows nothing about interfaces, BGP, or any particular YANG model. See the `Sensor` type and `SampleConfig` for the shape.

## The problem we are actually solving

The target scale is roughly **600 devices, polled every few minutes, each yielding hundreds-to-thousands of metrics**. At that scale two costs dominate, and the whole design is organised around them:

1. **SSH session setup is expensive.** The SSH handshake + NETCONF `<hello>` exchange is a real crypto/auth cost. Paying it *per poll* is ruinous at 600 devices.
2. **Resident memory of long-lived processes.** A persistent daemon carries a Go runtime + parser state. Multiply that by 600 and it stops being free.

Everything below follows from refusing to pay those two costs repeatedly.

## The core decisions

### One singleton daemon holding a warm session fleet

There is **one** process. It opens **one** SSH/NETCONF session per device and keeps it open across poll cycles. The session handshake is paid **once per device** (at first use), not once per poll. This is the single most important property of the design — it is the direct fix for cost (1).

The collection of all these persistent per-device sessions is what we informally call the *fleet* (originally, and misleadingly, "the pool" — see the rejected section on pooling).

### Session **reuse**, recycled only on a long timer

A session, once open, is reused indefinitely. It is torn down and re-established only in two cases:

- **On failure** — a failed RPC drops the session so the next cycle redials it.
- **On age** — a session older than `session_max_age` (default `1h`) is recycled. This is deliberate hygiene against silently-dead sessions, server-side idle timeouts, and credential rotation. It is a *long* timer by design; recycling is the exception, not the rhythm.

### "Model A runtime, Model B authoring"

This needs two definitions, because conflating them caused real confusion during design:

- **Model A** — *one* Telegraf plugin instance holds a *list* of devices. The plugin owns the fleet and fans out to devices itself.
- **Model B** — *one* plugin instance *per device*. Telegraf schedules each instance independently.

We run **Model A**, because a singleton warm fleet requires a single process that owns all the sessions. But Model A config (one giant block listing 600 devices) is miserable to author and maintain.

So we keep **Model B *authoring* ergonomics** via `devices_directory`: the one `[[inputs.netconf]]` block carries global settings and sensors, and each device is a small file dropped into a directory. `Init` reads the directory and merges every device into the single instance. You get per-device files (easy to template, generate, diff, add, remove) on top of a single-process runtime. Adding or removing a device is "drop/delete a file, reload Telegraf".

### Plain `Input`, lazy dial, inline recycle (not a background maintainer)

The plugin is a plain Telegraf `Input` (implements `Gather`), **not** a `ServiceInput` with a background connection-maintainer goroutine.

- On the first `Gather`, cold sessions are dialed (in parallel, bounded). This first cycle pays a one-time "cold-start dial storm" — accepted as inevitable and harmless.
- Every subsequent cycle finds the sessions already warm and just issues RPCs. This is what makes each steady-state poll fast and dial-free.
- Recycling (age/failure) happens *inline* in `Gather`: the affected device redials while the other 599 keep using their warm sessions, all in parallel.

The insight that killed the background maintainer: **session reuse already delivers "warm and ready" for every cycle after the first.** A maintainer would only additionally hide the very first cold start and the rare recycle — not worth the extra concurrency machinery, and it would opt us out of Telegraf's `precision` handling (which does not apply to service inputs).

### Concurrency model

- **Across devices: parallel.** `Gather` fans out one goroutine per device, bounded by `max_concurrent_devices` (default 10). Different devices dial and poll fully concurrently.
- **Within a device: serial.** A single `netconf.Session` is one SSH channel, and `Session.Exec` is an unlocked `Send` then `Receive`. Two concurrent RPCs on the same session would interleave and corrupt each other. So each device's sensors run **one at a time** on its session, guarded by a per-device mutex.
- **The bound matters most at warm-up and recycle** — that is where a dial storm (600 simultaneous SSH handshakes) would hit. Steady-state polling is 600 cheap RPCs on already-open channels.
- The Telegraf accumulator is channel-backed and safe for concurrent `AddMetric`/`AddError`, so device goroutines write to it freely.

### Self-timed cadence via `signal = "none"`

The `execd` parent is configured with `signal = "none"`, and our binary is run with `--poll_interval=<cadence>`. The parent never triggers us; it just consumes our stdout. **We** drive the poll cadence from the shim's own steady `time.Ticker`.

Why this and not `signal = "STDIN"` (where the parent triggers each interval): we want the cadence to live in *our* binary, independent of parent signaling — a persistent daemon that emits on its own schedule. This is the "service input" style, and it fits a warm-fleet daemon.

**Regular spacing is a ticker property, not a timestamp property.** `time.Ticker` fires on a fixed schedule and does not drift (a long poll may skip a tick, but lag never accumulates), so metrics land at a reliable interval. We do **not** compute or round timestamps ourselves; `precision`/`round_interval` only *snap* timestamps to boundaries like :00/:05, which is cosmetic and not required. The only thing protecting cadence is keeping `Gather` well under the interval — which warm sessions + bounded concurrency ensure. If a poll ever approaches the interval, the fix is a higher `max_concurrent_devices`, not touching timestamps.

## Rejected approaches

These are the ideas that looked right at some point and were consciously discarded. They are recorded so we do not "rediscover" them and repeat the mistakes.

### Why not one Telegraf instance per device (Model B) with `exec`?

> **[REJECTED]** This was the *previous* production setup and the reason this plugin exists.

The old approach used the `exec` plugin (not `execd`): one instance per device calling a script that opened a **fresh SSH session every poll**, collected everything, and closed. It was driven to large scale by brute force, but the repeated handshake was the wound. The large process count was *not* the main pain, because `exec` scripts are short-lived (spawn, dump, die, near-zero resident memory between polls).

The lesson carried into this design: the enemy is the per-poll handshake, not the device count. Keep sessions warm.

### Why not one `execd` instance per device?

> **[REJECTED]** Correct-looking (it is "just Model B on execd"), but it inverts the memory tradeoff badly.

Unlike `exec`, an `execd` process is a persistent daemon. Model B on `execd` therefore means ~600 permanently-resident daemons, each ~15–40 MB RSS → 10–20 GB of idle memory. `exec`'s short-lived processes never paid that. A single daemon holding 600 warm sessions (sockets + SSH state + a couple of goroutines each) is a few hundred MB total. The singleton wins on memory *and* on the handshake.

### Can we make `execd` behave like a game-engine singleton (start twice, attach to the existing instance)?

> **[REJECTED]** Category error: that pattern is in-process; `execd` is multi-process.

The classic singleton (uncontrolled instantiation from many call sites collapsing to one live object via a `Once`/mutex) relies on a shared heap. Each `[[inputs.execd]]` block is a separate OS process with its own heap; no static-instance trick spans them. Making "start again → attach to existing" work across processes would need OS-level IPC (lock file + named socket handoff) — real machinery and a new attack surface.

It is also unnecessary: the uncontrolled-instantiation problem never arises, because **config is the single call site**. One `[[inputs.execd]]` block = one process = the one instance. You do not deduplicate many starts; you simply define exactly one. Multiple execd blocks share no state and are not a way to build a shared fleet.

### Why not a background connection-maintainer goroutine (`ServiceInput`)?

> **[REJECTED]** Solves a problem that session reuse already solves.

A maintainer that pre-warms and proactively recycles would guarantee *zero* dial cost even on cold start. But reuse already makes every post-first cycle warm, and recycles are rare and cheap when done inline. The maintainer adds goroutine-lifecycle complexity and opts us out of `precision`. Net negative for the benefit.

### Why not a per-device connection pool (several sessions per device)?

> **[REJECTED]** Wrong scaling axis, and risky against real devices.

A pool of multiple sessions *to the same device* only helps when a single device has many slow sensors run concurrently — but a single session must serialise its RPCs anyway, and devices commonly cap concurrent NETCONF sessions (often single digits). The real win is concurrency **across** devices, which we already have. If one device's sensor set ever becomes the bottleneck, revisit this as an explicit, capped opt-in.

### Why not reuse Telegraf's `interval` / `collection_jitter` / `collection_offset` for our device fan-out?

> **[REJECTED]** They operate at the wrong granularity and do not even reach us under the shim.

Those knobs schedule *plugin instances*, not the devices inside one instance. Telegraf never sees our individual devices. Moreover, the `execd` shim ignores them entirely — it drives collection from its own `--poll_interval` ticker and decodes only the plugin table. At the parent level they still work on the `execd` wrapper (spacing it against other plugins), and if this plugin is ever compiled into a full agent it inherits them for free — so we must **not** reimplement them. Intra-fleet spreading is a separate concern handled by `max_concurrent_devices`.

### Why not a runtime hot-add control channel ("feed a new host into the running daemon")?

> **[REJECTED]** for now — buildable, but unjustified for a slow-changing inventory.

Hot-adding devices to the running daemon needs a control channel (unix socket / HTTP), concurrency-safe insertion, and an auth/attack surface to secure. Its only advantage over `devices_directory` + reload is avoiding a full-fleet redial when the inventory changes. Network-device inventories change slowly, so the reload storm is a rare, bounded, one-time event. Revisit only if inventory churn becomes frequent.

## Portability: what happens if this becomes a "proper" (compiled-in) plugin

Nothing about the internal design conflicts with a full Telegraf agent. All our logic lives inside `Init`/`Gather`/`Stop`, which the agent drives exactly as the shim does. The agent would apply `interval`/`jitter`/`offset` to *when* `Gather` is called; our device fan-out inside `Gather` remains opaque to it. Because we are a plain `Input` (not a `ServiceInput`), `precision` also applies normally in that world. The port is clean.

The only thing that is shim-specific is the *deployment* (`signal = "none"` + `--poll_interval`). In a compiled agent, cadence would come from the agent's `interval` instead — a config change, not a code change.

## Testability

Sessions sit behind a small `ncSession` interface (`Exec`, `Close`) that `*netconf.Session` satisfies, and the dialer is an injectable field. This lets unit tests exercise the connection manager — concurrency bound, recycle-on-age, drop-on-failure, device-directory merging — with fakes, without any real device. See the tests alongside the plugin.

## Known unknowns

- The interface-statistics sensor shipped as an example is spec-correct but **not yet validated against real hardware**; the `ietf-interfaces` operational data lives under `/interfaces-state` (RFC 7223) or `/interfaces` (RFC 8343, NMDA) depending on platform. This gets resolved when a device is available. See the README caveats.
