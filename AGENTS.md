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
  than YAML. Do not add a dependency without raising it first. The one runtime
  dependency is `ffmpeg`, shelled out to for HLS remuxing only; it is optional
  (devices and direct streams never need it) and its absence is a startup
  warning, not a hard failure.
- **Layout.** `main.go` at the root stays thin; real work lives in
  `internal/{config,device,hdhr}`.
- **`internal/hdhr` is control-plane only.** It speaks to the devices' JSON
  endpoints. Streaming lives in `internal/stream`, deciding which tuner to use
  lives in `internal/arbiter`, and sharing one upstream among consumers lives in
  `internal/relay` — because a device allocates a tuner per connection, stream
  lifetime is an arbitration concern, not a client one.
- **`internal/relay` owns the lease for a shared stream.** One `arbiter.Lease`
  is held per upstream (`broadcast`), not per consumer, and released by the
  reader's `finish` when the stream tears down. Nothing outside the hub should
  acquire or release a lease on a streaming path. If open fails during `create`,
  that candidate's lease is released before trying the next.
- **The grace period holds a tuner past the last consumer.** When a broadcast
  empties it does not tear down immediately; a timer (`broadcast.grace`) keeps
  the upstream open so a returning consumer reattaches without re-tuning. `join`
  cancels the timer; the reader keeps reading and discarding during the window.
  Grace `0` restores immediate teardown. All state changes (`draining`,
  `closed`, the timer) are under `broadcast.mu`; the timer callback re-checks
  emptiness under the lock, since a `join` can race it.
- **Web stream candidates take no tuner.** A candidate with `Web: true` skips
  `arbiter.TryAcquire` in `create` and its `broadcast.lease` is nil (which
  `Lease.Release` guards). It ranks below every tuner in `rankCandidate`, so it
  serves only when the tuners are exhausted, and it carries `Headers` through
  the `relay.Source` for streams that require a specific Referer or User-Agent.
- **HLS web streams remux through ffmpeg; direct streams relay bytes.** The
  `relay.Source.Remux` flag (resolved from `Stream.RemuxEnabled`, auto-detecting
  `.m3u8`) selects `stream.FFmpeg` over `stream.Proxy` in the server's
  `streamOpener`. `FFmpeg.Open` spawns `ffmpeg -c copy -f mpegts` and returns
  its stdout as the upstream; `Close` kills the process, which unblocks the
  reader — the same contract a device connection has. Headers must precede `-i`
  so ffmpeg applies them to the HLS segment requests too, which is the whole
  point. `ffmpeg` reaping happens exactly once, guarded, since both `Read` (on
  EOF, to surface stderr) and `Close` may call `Wait`.
- **The reuse key is the upstream URL, not the channel number.** Two presented
  channels that resolve to the same device feed must share one tuner. Keying by
  channel number would open a second tuner to a feed already being received.
- **The upstream must outlive the request that started it.** `create` opens with
  `context.Background()`, never the requester's context — otherwise the consumer
  who happened to start a shared stream would tear it down for everyone by
  disconnecting. Per-consumer lifetime is the subscriber, not the upstream.
- **One reader per upstream, fresh buffer per read.** The chunk is fanned out to
  every subscriber and read concurrently, so it must not be overwritten by the
  next read. Sends to subscribers are non-blocking; a subscriber whose buffer
  fills is dropped, so one slow consumer can never stall the reader or others.
- **Never set a write timeout on the HTTP server or the stream client.** A
  stream stays open for as long as someone is watching; a deadline would cut it
  off mid-programme. Only connect and response-header phases are bounded.
- **A cancelled request is a normal stream ending, not an error.** Consumers
  disconnect constantly; the `pump` loop treats a closed chunk channel or a
  write error as a routine end, not a failure to log.
- **Never set a write timeout on the HTTP server or the stream client.** A
  stream stays open for as long as someone is watching; a deadline would cut it
  off mid-programme. Only connect and response-header phases are bounded.
- **A cancelled request is a normal stream ending, not an error.** Consumers
  disconnect constantly, which cancels the request context and surfaces as a
  read error. Treating it as a failure buries the real errors.
- **Tests use verbatim device JSON.** Fixtures in `internal/hdhr/types_test.go`
  are real captures, trimmed and with `DeviceAuth` redacted. Keep new fixtures
  real; the field-presence quirks below are the entire reason these tests exist.

## Device behaviour worth knowing

Established empirically against real hardware (a CableCARD tuner and an ATSC
1.0/3.0 tuner) in July 2026. Figures below are from that one fleet and are
illustrative — the *rules* are what matter, not the counts. Re-verify against
whatever hardware is to hand if behaviour looks different, especially after a
firmware change.

- **One tuner per HTTP connection, not per channel.** Two concurrent connections
  to the same channel allocate two tuners. Confirmed on both a CableCARD and an
  ATSC device. This is why a 302-redirect design cannot dedupe streams and why
  Sourcery must proxy. Do not reintroduce a redirect-based shortcut.
- **`VctNumber` in `status.json` is not a reliable key.** It reports what the
  tuned stream advertises about itself, not the GuideNumber used to reach it —
  an observed tuner reported `VctNumber 232` / `VctName WDIVDT` while the lineup
  listed that station at GuideNumber 4. Track upstream connections directly; use
  `status.json` only for occupancy counts and out-of-band detection.
- **Idle tuners report only `Resource`;** every other field is omitted. Absence
  of `Frequency` is the sole idle/active signal, hence `Tuner.Active()`.
- **Tuners free within ~2s of disconnect,** so `status.json` polling is a sound
  basis for accounting.
- **Consumers bypass Sourcery**, and this is normal rather than a fault.
  Capacity must always be derived from device `status.json`, never from
  Sourcery's own bookkeeping alone, or the arbiter will over-commit.
- **Optional lineup fields really are optional.** `DRM` appears only on
  protected entries; `SignalStrength` and `SignalQuality` came from the antenna
  device alone. Everything optional must decode to zero rather than being
  required, and zero means "not reported" as much as it means "no".
- **ATSC 3.0 is excluded by default.** Its AC4 audio does not play reliably on
  current consumers. Devices tend to number these above the ATSC 1.0 channels
  they shadow (102.1 shadowing 2.1, and so on), so dropping them typically costs
  nothing. `allow_atsc3` turns them back on, in which case codec compatibility
  outranks source preference in `rankCandidate` so they stay below their
  equivalents.
- **Detect ATSC 3.0 by HEVC video or AC4 audio, never by H264.** Cable carries
  large numbers of ordinary H264/AC3 channels — over a third of the lineup on
  the observed fleet — and treating that codec as next-generation would exclude
  all of them.
- **ATSC 3.0 mobile feeds are excluded separately.** Companion streams for
  handheld devices appear on a `.99` subchannel with a `MOB` callsign suffix.
  Redundant while ATSC 3.0 is off wholesale, but it keeps them out if it is ever
  turned on.
- **The spine supplies identity; other devices supply routes.** The presented
  number and callsign always come from the most comprehensive lineup (cable,
  where present), because that is what the consumers' guide data is built
  around. Other devices attach as *routing candidates* and may be preferred for
  streaming, so the substitution is invisible. Never let a routing preference
  change what a channel is called — that breaks guide data.
- **Spine channels are never merged with each other.** A provider may list a
  station's standard and high definition feeds as separate channels with
  separate guide data; pass that through untouched. One antenna feed may attach
  to several spine channels, and should.
- **Duplicates are pervasive.** Callsigns repeat within a single lineup — one
  observed cable lineup had 179 repeated names, and one antenna carried seven
  distinct subchannels sharing a callsign. Non-spine channels are appended
  individually; never collapse them by name. An earlier name-only merge silently
  deleted six subchannels this way.
- **Sources name the same station differently.** Broadcast lineups suffix
  callsigns with a transmission type (`WDIV-HD`, `CHWI_HD`, `WUDT-LD`); cable
  uses the bare callsign (`WDIV`) plus a `DT`-suffixed high definition variant
  (`WDIVDT`). `normalizeName` strips a separator-delimited suffix and a bare
  trailing `DT`. Guards that matter: the suffix needs a separator, so names like
  `QVCHD` and `QVC` — genuinely different channels on cable — do not collapse;
  `DT` trimming requires a three-character stem, so `WUDT` is not cut to `WU`;
  and `WDIVDT2` keeps its digit, so subchannels are untouched.
- **Some stations will never match automatically.** One observed antenna feed is
  called `H&I` where cable calls the same programming `WDIVDT2`. That is what
  manual mapping is for (`mappings` in config); do not widen the fuzzy matching
  to chase them. A mapped source attaches only where the mapping says — it is
  held out of automatic matching and does not stand alone — and an unmatched
  mapping is reported (`Lineup.UnmatchedMappings`), never silently dropped.
- **Both device types report the HD flag**, and on cable it tracked the H264 set
  exactly. Ranking prefers HD above source preference: a viewer notices standard
  definition and does not notice which tuner produced the picture.
- **Device IDs carry a checksum.** Consumers reject malformed ones, so the
  emulated ID must validate. See `internal/hdhr/deviceid.go`; the algorithm is
  verified in tests against IDs known to be valid, and against corruptions of
  them that must not be.
- **Stream paths carry a `v` prefix.** `/auto/v2.1` means "tune virtual channel
  2.1"; the `v` is not part of the number.

## Tuner accounting

- **Capacity is `tuners - held - foreign`.** `held` is what Sourcery opened and
  is authoritative and immediate. `foreign` is everything else, learned by
  polling `status.json` every five seconds.
- **`status.json` counts Sourcery's own streams too**, so foreign usage is
  `inUse - held`, clamped at zero. The two figures are sampled at slightly
  different moments and can briefly disagree; a negative result must never
  become spare capacity.
- **The device is the final authority.** The poll is always stale, so a device
  may refuse a tuner the arbiter believed was free. Release the lease and try
  the next candidate rather than failing the request.
- **Never evict a stream in flight.** When nothing is available, answer 503 and
  log it. Contention is meant to be visible, not silently resolved.

## Handling secrets

`discover.json` returns a `DeviceAuth` token used for Silicondust's cloud and
DVR services. Keep it out of logs, fixtures, and anything user-visible.

## Testing against real hardware

The devices are live and shared. Opening streams consumes real tuners and can
starve an in-progress recording, so check `status.json` for free capacity first,
abort if the headroom is not there, and keep test streams short. Prefer whichever
device has the most spare tuners, and avoid the scarcer cable device — that is
where contention actually hurts.
