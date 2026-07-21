# Sourcery

A thin tuner arbiter for HDHomeRun devices.

Sourcery emulates an HDHomeRun tuner, sits in front of the real devices, and
arbitrates their tuners across consumers that will not coordinate with each
other — here, two Channels DVR instances and one Plex instance. It gives those
consumers a single unified lineup, reuses an upstream stream when more than one
of them wants the same channel, and prefers antenna over cable to conserve the
scarcer cable tuners.

Written in Go with no dependencies outside the standard library, targeting a
Raspberry Pi.

## Why

Three consumers, seven tuners, and no shared view of who is using what:

| Device | Address | Tuners | Lineup |
|---|---|---|---|
| HDHomeRun PRIME (HDHR3-CC) | 192.0.2.11 | 3 | 492 cable channels |
| HDHomeRun FLEX 4K (HDFX-4K) | 192.0.2.10 | 4 | 70 antenna channels |

Each consumer talks to the devices directly and independently, so they collide
and exhaust the tuner budget.

The catch that shapes the whole design: **an HDHomeRun allocates one tuner per
HTTP connection, not per channel.** Two connections to the same channel consume
two tuners — measured on both devices. So Sourcery cannot simply redirect
consumers to the devices; it has to stay in the data path and fan the transport
stream out to multiple subscribers. See [PLAN.md](PLAN.md) for the full
reasoning and the milestone breakdown.

## Status

**M1 — Sourcery serves a discoverable tuner with a merged lineup.** Streaming
is not implemented yet; stream requests return 503.

- [x] **M0** Config, device registry, one-shot probe
- [x] **M1** Lineup merge and HDHomeRun emulation endpoints
- [ ] **M2** Passthrough proxy and arbitration
- [ ] **M3** Stream reuse via fan-out
- [ ] **M4** Status reconciliation and source preference
- [ ] **M5** Manual lineup overrides and operability

## Usage

```sh
go build -o sourcery .
./sourcery -config config.json          # serve the emulated tuner
./sourcery -config config.json -probe   # report device state and exit
```

Point Plex or Channels DVR at Sourcery's address as though it were an
HDHomeRun. It serves `discover.json`, `lineup.json`, `lineup_status.json` and
`device.xml` on port 5004.

The probe report:

```
DEVICE  ADDRESS        SOURCE   MODEL     TUNERS      CHANNELS  DRM
flex    192.0.2.10  antenna  HDFX-4K   3 free / 4  70        3
prime   192.0.2.11  cable    HDHR3-CC  3 free / 3  492       7

6 of 7 tuners free
  flex/tuner2 in use by 192.0.2.30 (reports channel 2.1 WJBK)

merged lineup: 535 channels, 15 available from more than one source
excluded: 10 copy-protected, 2 ATSC 3.0 mobile feeds
  2.1      WJBK         flex:2.1 ->  prime:2
  4.1      WDIV-HD      flex:4.1 ->  prime:4
  7.1      WXYZ-HD      flex:7.1 ->  prime:7
```

The `in use by` line is a consumer streaming *directly* from a device,
bypassing Sourcery. Tuner accounting is derived from each device's
`status.json` rather than from Sourcery's own bookkeeping, precisely so that
traffic it did not originate still counts against capacity.

## How the lineup is merged

562 channels across the two devices become 533, with every station reachable
from both sources listed once and its alternatives ranked.

Entries merge **only across devices, never within one**: seven distinct
WDWO-CD subchannels share a callsign on the antenna, and collapsing those by
name would delete six channels. Names are matched after dropping a
transmission-type suffix, so the antenna's `WDIV-HD` meets cable's `WDIV`.

Candidates for a channel are ranked by playable codec first, then antenna
before cable, then lowest channel number. Codec outranks source preference
because a stream that will not play is worth nothing, however cheap its tuner.

Dropped entirely:

- **ATSC 3.0**, whose AC4 audio does not play reliably on the consumers. Every
  such channel here shadows an ATSC 1.0 twin that does play, so nothing is lost.
  Set `"allow_atsc3": true` to admit them; they are ranked below their twins
  rather than preferred.
- **Copy-protected channels**, which the consumers cannot play at all.
- **ATSC 3.0 mobile companion feeds** — `.99` subchannels with `MOB` callsigns,
  meant for handheld devices. Redundant while ATSC 3.0 is off wholesale, but it
  keeps them out if it is ever turned on.

ATSC 3.0 is identified by HEVC video or AC4 audio. H264 is *not* a signal:
cable carries 184 ordinary H264/AC3 channels, and treating that codec as
next-generation would exclude a third of the lineup.

## Configuration

`config.json` lists the devices to manage. Unknown keys are rejected at startup,
so a typo fails loudly instead of being ignored.

```json
{
  "devices": [
    { "name": "flex",  "address": "192.0.2.10", "source": "antenna" },
    { "name": "prime", "address": "192.0.2.11", "source": "cable" }
  ]
}
```

`source` must be `antenna` or `cable`; it drives source preference when a
channel is available from both.

Optional keys: `listen` (default `:5004`), `friendly_name` (default
`Sourcery`), `tuner_count` (default 7), `device_id` to pin the advertised
identity, and `advertise_address` to override the host used in stream URLs when
the request's own host is not reachable by consumers.

## Cross-compiling for the Pi

```sh
GOOS=linux GOARCH=arm64 go build -o sourcery .   # Pi 4 / Pi 5, 64-bit
GOOS=linux GOARCH=arm   go build -o sourcery .   # 32-bit
```
