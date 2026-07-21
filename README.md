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

**M0 — device registry and probing.** Sourcery does not serve anything yet.

- [x] **M0** Config, device registry, one-shot probe
- [ ] **M1** Lineup merge and HDHomeRun emulation endpoints
- [ ] **M2** Passthrough proxy and arbitration
- [ ] **M3** Stream reuse via fan-out
- [ ] **M4** Status reconciliation and source preference
- [ ] **M5** Manual lineup overrides and operability

## Usage

```sh
go build -o sourcery .
./sourcery -config config.json
```

```
DEVICE  ADDRESS        SOURCE   MODEL     TUNERS      CHANNELS  DRM
flex    192.0.2.10  antenna  HDFX-4K   3 free / 4  70        3
prime   192.0.2.11  cable    HDHR3-CC  3 free / 3  492       7

6 of 7 tuners free
  flex/tuner2 in use by 192.0.2.30 (reports channel 2.1 WJBK)
```

That last line is a consumer streaming *directly* from a device, bypassing
Sourcery. Tuner accounting is derived from each device's `status.json` rather
than from Sourcery's own bookkeeping, precisely so that traffic it did not
originate still counts against capacity.

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

## Cross-compiling for the Pi

```sh
GOOS=linux GOARCH=arm64 go build -o sourcery .   # Pi 4 / Pi 5, 64-bit
GOOS=linux GOARCH=arm   go build -o sourcery .   # 32-bit
```
