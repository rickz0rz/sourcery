# Sourcery — agent notes

Read [PLAN.md](PLAN.md) before making design decisions; it carries the
architecture and milestone breakdown. This file covers conventions and the
findings that are expensive to rediscover.

## Commands

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .                                       # must print nothing
./sourcery -config config.json                   # probe the real devices
GOOS=linux GOARCH=arm64 go build -o sourcery .   # Pi target
```

## Conventions

- **Standard library only.** The binary ships to a Pi and should stay a single
  static file with no module dependencies. This is why config is JSON rather
  than YAML. Do not add a dependency without raising it first.
- **Layout.** `main.go` at the root stays thin; real work lives in
  `internal/{config,device,hdhr}`.
- **`internal/hdhr` is control-plane only.** It speaks to the devices' JSON
  endpoints. Streaming deliberately does not belong here — because a device
  allocates a tuner per connection, stream lifetime is an arbitration concern
  and belongs with the proxy (M2/M3).
- **Tests use verbatim device JSON.** Fixtures in `internal/hdhr/types_test.go`
  are real captures, trimmed and with `DeviceAuth` redacted. Keep new fixtures
  real; the field-presence quirks below are the entire reason these tests exist.

## Device behaviour worth knowing

These were established empirically against the real hardware on 2026-07-20.
Re-verify if firmware changes (PRIME was on 20260313, FLEX 4K on 20260326).

- **One tuner per HTTP connection, not per channel.** Two concurrent connections
  to the same channel allocate two tuners. Confirmed on both the FLEX 4K
  (`/auto/v2.1` → tuner2 + tuner3) and the PRIME (`/auto/v3` → tuner1 +
  tuner2). This is why a 302-redirect design cannot dedupe streams and why
  Sourcery must proxy. Do not reintroduce a redirect-based shortcut.
- **`VctNumber` in `status.json` is not a reliable key.** It reports what the
  tuned stream advertises about itself, not the GuideNumber used to reach it —
  an observed tuner showed `VctNumber 232` / `VctName WDIVDT` while the lineup
  lists WDIV at GuideNumber 4. Track upstream connections directly; use
  `status.json` only for occupancy counts and out-of-band detection.
- **Idle tuners report only `Resource`;** every other field is omitted. Absence
  of `Frequency` is the sole idle/active signal, hence `Tuner.Active()`.
- **Tuners free within ~2s of disconnect,** so `status.json` polling is a sound
  basis for accounting.
- **Consumers bypass Sourcery.** Something on the LAN streams straight from the
  PRIME. Capacity must always be derived from device `status.json`, never from
  Sourcery's own bookkeeping alone, or the arbiter will over-commit.
- **Lineup fields are inconsistent across models.** The antenna lineup carries
  `HD`, `SignalStrength` and `SignalQuality`; the cable lineup carries none of
  them. `DRM` appears only on protected entries. Everything optional must decode
  to zero rather than being required.
- **The antenna's 100+ channels are ATSC 3.0, and they are excluded by
  default.** Their AC4 audio does not play reliably on the consumers. All seven
  shadow an MPEG2/AC3 ATSC 1.0 twin that does play (102.1 shadows 2.1, 104.1
  shadows 4.1, and so on), so dropping them costs nothing — all 15 cross-source
  merges survive. `allow_atsc3` in config turns them back on, in which case
  codec compatibility outranks source preference in `rankCandidate` so they
  stay below their twins.
- **Detect ATSC 3.0 by HEVC video or AC4 audio, never by H264.** Cable carries
  184 ordinary H264/AC3 channels; treating that codec as next-generation would
  exclude a third of the lineup. Codec counts across the fleet: cable is 308
  MPEG2 and 184 H264; antenna is 62 MPEG2, 7 HEVC, 1 H264.
- **ATSC 3.0 mobile feeds are excluded separately.** Companion streams for
  handheld devices appear on a `.99` subchannel with a `MOB` callsign suffix
  (107.99 WXYZMOB, 120.99 WMYDMOB). Redundant while ATSC 3.0 is off wholesale,
  but it keeps them out if it is ever turned on.
- **Duplicates are pervasive on both sources.** 179 cable names appear more than
  once (GVACC2 at 5, 915 and 1090); the antenna carries seven distinct WDWO-CD
  subchannels at 18.1 through 18.7. **Merge across devices only, never within
  one** — two numbers on one device are two channels even when the names match,
  and collapsing them silently deletes channels. This bit once already.
- **The two sources name stations differently.** The antenna suffixes callsigns
  with a transmission type (`WDIV-HD`, `CHWI_HD`, `WDWO-CD`, `WUDT-LD`); cable
  uses the bare callsign (`WDIV`). `normalizeName` strips such a suffix, but
  only when a separator sets it off — cable's own `QVCHD` and `QVC` are
  genuinely different channels and must not collapse.
- **15 channels genuinely overlap both sources**, including all the main locals.
  Source preference is still the narrower optimization; stream reuse is the
  broad one, and the two should not be conflated.
- **Device IDs carry a checksum.** Consumers reject malformed ones, so the
  emulated ID must validate. See `internal/hdhr/deviceid.go`; the algorithm is
  verified against both real device IDs in tests.
- **Stream paths carry a `v` prefix.** `/auto/v2.1` means "tune virtual channel
  2.1"; the `v` is not part of the number.

## Handling secrets

`discover.json` returns a `DeviceAuth` token used for Silicondust's cloud and
DVR services. Keep it out of logs, fixtures, and anything user-visible.

## Testing against real hardware

The devices are live and shared. Opening streams consumes real tuners and can
starve an in-progress recording, so check `status.json` for free capacity first
and keep test streams short. Prefer the FLEX (4 tuners, usually idle) over the
PRIME (3 tuners, cable, and where contention actually hurts).
