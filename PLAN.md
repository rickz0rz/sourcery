# Sourcery — Plan

A thin Go service that emulates an HDHomeRun tuner, sits in front of the real
devices, and arbitrates their tuners across consumers that refuse to coordinate
with each other.

## Problem

Several consumers — DVR software such as Channels and Plex — each talk to the
HDHomeRun devices directly and independently. They have no shared view of tuner
usage, so they collide and exhaust a small tuner budget.

The reference deployment this plan was written against is a CableCARD tuner
(three tuners, a large cable lineup) alongside an ATSC 1.0/3.0 tuner (four
tuners, a smaller broadcast lineup): seven tuners shared between three
consumers. Nothing in the design depends on that particular arrangement.

## The constraint that shapes everything

**HDHomeRun devices allocate one tuner per HTTP connection, not per channel.**
Measured on both devices — two concurrent connections to the same channel
consume two tuners:

```
cable    /auto/v3    -> tuner1 (VctNumber 3) + tuner2 (VctNumber 3)
antenna  /auto/v2.1  -> tuner2 (VctNumber 2.1) + tuner3 (VctNumber 2.1)
```

This rules out the originally-sketched 302-redirect design. A redirect puts
Sourcery outside the data path: the consumer opens its own connection to the
device, and the device does the naive thing. **Stream reuse requires Sourcery to
stay in the data path and fan out the transport stream.**

Both devices release tuners within ~2s of disconnect, so `status.json` polling is
viable for accounting.

## Two distinct optimizations

Worth keeping separate; they have very different reach.

1. **Stream reuse** — N consumers wanting the same channel share one upstream
   tuner. Applies to *any* of the ~560 channels. This is the main win.
2. **Source preference** — prefer antenna over cable when a channel exists on
   both, to conserve the scarcer 3-tuner cable budget. Only ~6 channels
   genuinely qualified on the reference fleet; the rest of the overlap was
   shopping channels.

## Architecture

Single Go binary, no external dependencies, targeting a Pi 4/5 (gigabit NIC).

```
  Channels A ─┐
  Channels B ─┼─> Sourcery :5004 ─── arbiter ───> cable device
  Plex       ─┘   (HDHR emulation)    │      └──> antenna device
                                       └─ status poller
```

**Components**

- **Device registry** — configured device list; polls `discover.json` for
  identity and tuner count.
- **Lineup merger** — fetches both `lineup.json`, normalizes names, groups
  entries believed to be the same logical channel, and ranks candidate sources
  per channel (antenna first). Auto-merge with manual overrides in config.
- **HDHomeRun frontend** — serves `discover.json`, `lineup.json`,
  `lineup_status.json`, and stream URLs, so consumers see one virtual tuner.
- **Arbiter** — on each stream request: reuse an existing upstream stream if one
  is already open for that logical channel; otherwise pick the best-ranked
  source with a free tuner; otherwise 503.
- **Fan-out proxy** — one upstream connection per unique channel, N subscribers.
- **Status poller** — reconciles against each device's `status.json`.

## Design notes worth getting right

**Out-of-band usage is real.** During testing, another host on the network was
streaming directly from a device, bypassing Sourcery entirely. Tuner accounting
must be derived from the devices' `status.json`, not from Sourcery's own
bookkeeping alone — otherwise it will over-commit. Available = `TunerCount` −
(in use per device).

**`VctNumber` is not a reliable key.** `status.json` reports the channel number
the *stream* advertises, not the one requested. An observed tuner reported
`VctNumber 232` / `VctName WDIVDT` while the lineup listed it at GuideNumber 4.
Sourcery should track its own upstream connections directly and use `status.json`
only for total occupancy and out-of-band detection — never to identify which
managed stream holds a tuner.

**Duplicate channels within a single source.** 179 cable names appear more than
once, and an antenna may list one station at two different numbers.
Merge output should be a ranked *list* of `(device, guideNumber)` candidates per
logical channel, so the arbiter can fall through when its first pick is busy.

**Slow consumers must not stall the upstream.** Fan-out needs per-subscriber
buffering with an explicit drop-or-disconnect policy; the upstream reader can
never block on the slowest subscriber.

**Probe and channel-surf churn.** DVRs open, sample, and close streams during
setup and scanning. Tearing an upstream down the instant the last subscriber
leaves would thrash tuners. Keep upstreams alive for a short grace period after
the last subscriber disconnects.

**Advertised tuner count.** Because reuse lets more concurrent consumers than
physical tuners succeed, the emulated `TunerCount` need not be 7. Make it
configurable; start at 7 and revisit once reuse is proven.

**DRM channels** should be excluded or flagged — the consumers can't play them
anyway.

## Milestones

- **M0 — Skeleton.** Config, device registry, probe `discover.json` /
  `lineup.json` / `status.json`. No serving yet.
- **M1 — Lineup + emulation.** Merge lineups, serve the HDHomeRun discovery
  endpoints. Success: Plex and both Channels instances detect Sourcery and
  complete a channel scan.
- **M2 — Passthrough proxy.** One subscriber per upstream, no reuse. Arbiter
  picks a source and 503s at capacity. Success: live playback works end to end
  through Sourcery on all three consumers.
- **M3 — Reuse.** Fan-out to N subscribers per upstream. Success: three
  consumers on one channel occupy exactly one physical tuner.
- **M4 — Reconciliation.** Status polling, out-of-band usage detection, grace
  periods, source preference (antenna first).
- **M5 — Operability.** Manual lineup overrides, and a status view showing which
  consumer holds which channel on which physical tuner.

## Open questions

- **Consumer identity.** Source IP is the obvious handle (and `status.json`
  exposes `TargetIP` for out-of-band streams). Currently planned for logging and
  observability only — no per-consumer quotas or priority. Revisit if you want
  Plex to yield to Channels recordings.
- **Contention policy.** Settled for now: refuse with 503, never evict an
  in-flight stream. M2 should log contention events so the policy can be
  revisited with real data.
