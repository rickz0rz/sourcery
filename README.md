# Sourcery

A thin tuner arbiter for HDHomeRun devices.

Sourcery emulates an HDHomeRun tuner, sits in front of your real devices, and
arbitrates their tuners across consumers that will not coordinate with each
other — Plex, Channels DVR, and anything else that speaks the HDHomeRun
protocol. It gives them a single unified lineup, prefers whichever source is
cheapest and best for each channel, and refuses cleanly when nothing is free.

Written in Go with no dependencies outside the standard library, small enough
to run on a Raspberry Pi.

## Why

Tuners are a scarce shared resource, and DVR software does not share them.
Point two or three consumers at the same devices and they will each grab tuners
without regard for one another, until somebody's recording fails.

Sourcery gives them one address to talk to and makes the allocation decisions
itself.

The constraint that shapes the design: **an HDHomeRun allocates one tuner per
HTTP connection, not per channel.** Two connections to the same channel consume
two tuners. So Sourcery cannot simply redirect consumers to a device — it has
to stay in the data path and relay the stream. See [PLAN.md](PLAN.md) for the
reasoning and the milestone breakdown.

## Status

**M3 — Sourcery shares tuners.** Consumers watching the same channel share one
upstream connection, so three consumers of one channel occupy one tuner instead
of three.

- [x] **M0** Config, device registry, one-shot probe
- [x] **M1** Lineup merge and HDHomeRun emulation endpoints
- [x] **M2** Passthrough proxy and arbitration
- [x] **M3** Stream reuse via fan-out
- [ ] **M4** Grace periods and richer reconciliation
- [ ] **M5** Manual lineup overrides and operability

## Getting started

Copy the example configuration and fill in your own devices:

```sh
cp config.example.json config.json
$EDITOR config.json
go build -o sourcery .
./sourcery -probe          # check what Sourcery can see
./sourcery                 # serve the emulated tuner
```

Then point Plex or Channels DVR at Sourcery's address as though it were an
HDHomeRun. It serves `discover.json`, `lineup.json`, `lineup_status.json` and
`device.xml`, by default on port 5004.

## Configuration

Any number of devices, each labelled with where it gets its signal:

```json
{
  "devices": [
    { "name": "antenna", "address": "192.0.2.10", "source": "antenna" },
    { "name": "cable",   "address": "192.0.2.11", "source": "cable" }
  ]
}
```

`source` must be `antenna` or `cable`. It drives source preference: when a
channel is available from both, the antenna is preferred, because cable tuners
are usually the scarcer resource. Unknown keys are rejected at startup, so a
typo fails loudly instead of being silently ignored.

Everything else is optional:

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:5004` | Address to serve on |
| `friendly_name` | `Sourcery` | How Sourcery introduces itself |
| `tuner_count` | `7` | Tuner count to advertise |
| `device_id` | derived | Pin the advertised identity (eight hex digits) |
| `advertise_address` | request host | Override the host used in stream URLs |
| `allow_atsc3` | `false` | Admit ATSC 3.0 channels |

## The probe report

`./sourcery -probe` reports what Sourcery can see and exits:

```
DEVICE   ADDRESS     SOURCE   MODEL     TUNERS      CHANNELS  DRM
antenna  192.0.2.10  antenna  HDFX-4K   3 free / 4  70        3
cable    192.0.2.11  cable    HDHR3-CC  3 free / 3  492       7

6 of 7 tuners free
  antenna/tuner2 in use by 192.0.2.50 (reports channel 2.1 WJBK)

merged lineup: 531 channels, 35 available from more than one source
excluded: 7 ATSC 3.0, 7 copy-protected, 0 mobile feeds
  2      WJBK     antenna:2.1 -> cable:2
  16     HSN      antenna:20.4 -> antenna:31.8 -> cable:16
```

The `in use by` line is a consumer streaming directly from a device, bypassing
Sourcery. Tuner accounting is derived from each device's `status.json` rather
than from Sourcery's own bookkeeping, precisely so that traffic it did not
originate still counts against capacity.

Each lineup row is a channel with more than one source, in the order Sourcery
will try them.

## How the lineup is merged

**Identity and routing are separate concerns.** Whichever source is more
comprehensive — usually cable — becomes the spine, and every channel keeps that
source's number and callsign, so the lineup matches the guide data consumers
fetch. Other devices attach as alternative *routes* to the same channel and may
be preferred for streaming. A consumer asking for a cable channel may be served
from the antenna without ever knowing an antenna exists.

Spine channels are never merged with each other. A cable provider may list a
station's standard and high definition feeds as separate channels with separate
guide data; Sourcery passes that through untouched, while one antenna feed can
attach to both.

Channels that exist only on a non-spine device are appended, each standing on
its own.

Names are matched after dropping a transmission-type suffix, since broadcast
and cable lineups name the same station differently — `WDIV-HD` over the air
against `WDIV` and `WDIVDT` on cable. The matching is deliberately conservative:
under-merging costs a routing option, while over-merging sends a consumer to
the wrong programme. Stations that never match automatically are what manual
mapping (M5) is for.

Candidates are ranked by playable codec, then high definition, then antenna
before cable, then lowest channel number. Codec comes first because a stream
that will not play is worth nothing however cheap its tuner; picture quality
comes before tuner economy because a viewer notices standard definition and
will not notice which tuner produced the picture.

Dropped entirely:

- **ATSC 3.0**, whose AC4 audio does not play reliably on current consumers.
  Where these channels shadow an ATSC 1.0 equivalent, nothing is lost. Set
  `"allow_atsc3": true` to admit them; they rank below their equivalents rather
  than above.
- **Copy-protected channels**, which the consumers cannot play at all.
- **ATSC 3.0 mobile companion feeds** — `.99` subchannels with `MOB` callsigns,
  intended for handheld devices.

ATSC 3.0 is identified by HEVC video or AC4 audio. H264 is deliberately not a
signal, since cable carries large numbers of ordinary H264 channels.

## How a stream is served

A consumer asks Sourcery for a channel. Sourcery walks that channel's
candidates in preference order, claims a tuner on the first device with one
free, opens the upstream stream, and relays it:

```
msg=streaming consumer=192.0.2.50 channel=2 name=WJBK device=antenna source=antenna device_channel=2.1
msg="stream ended" consumer=192.0.2.50 channel=2 device=antenna bytes=9695860 seconds=9
```

Capacity has two parts. Sourcery knows exactly what it is holding, because it
opened it. What *else* is using the devices is learned by polling each
`status.json` every few seconds — consumers that bypass Sourcery are a normal
condition, and their tuners must count or the arbiter will hand out tuners that
do not exist.

That poll is always slightly stale, so the device is the final authority: if
opening a stream fails, Sourcery releases the claim and tries the next
candidate. When no candidate works it answers 503 rather than tearing down a
stream that is already playing. Nothing in flight is ever interrupted, and the
refusal is logged so contention is visible.

## How tuners are shared

When a consumer asks for a channel that Sourcery is already receiving, it joins
the existing stream rather than opening a second connection — so three
consumers of one channel occupy one tuner:

```
flex ch 2.1  3 consumer(s) on 1 tuner
```

The reuse key is the upstream feed, not the channel number, so two different
listings that resolve to the same device feed — a station's standard and high
definition entries, say — also share a tuner. Requests that arrive at the same
instant for the same channel converge on one connection rather than racing to
open several.

Reuse is preferred over source preference: joining an open stream costs no tuner
at all, which conserves capacity better than picking the nominally preferred
source would.

One reader pulls from each upstream and fans its bytes out to every consumer. A
consumer that cannot keep up with the real-time stream is dropped rather than
allowed to stall the reader or the others; its player can reconnect. When the
last consumer of a stream leaves, the upstream is closed and its tuner released.

## Cross-compiling for a Raspberry Pi

```sh
GOOS=linux GOARCH=arm64 go build -o sourcery .   # 64-bit
GOOS=linux GOARCH=arm   go build -o sourcery .   # 32-bit
```
