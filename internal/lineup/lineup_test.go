package lineup

import (
	"errors"
	"testing"

	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
)

func state(name string, src config.Source, chans ...hdhr.Channel) device.State {
	return device.State{
		Device: &device.Device{Device: config.Device{Name: name, Source: src}},
		Lineup: chans,
	}
}

func ch(number, name, video string) hdhr.Channel {
	return hdhr.Channel{GuideNumber: number, GuideName: name, VideoCodec: video, AudioCodec: "AC3"}
}

// A manual mapping connects stations that automatic matching cannot -- an
// antenna feed named "H&I" that cable lists as "WDIVDT2".
func TestManualMappingAttachesSource(t *testing.T) {
	states := []device.State{
		state("cable", config.SourceCable, ch("294", "WDIVDT2", "MPEG2")),
		state("antenna", config.SourceAntenna, ch("4.2", "H&I", "MPEG2")),
	}
	opts := Options{Mappings: []config.Mapping{
		{Channel: "294", Source: config.ChannelRef{Device: "antenna", Channel: "4.2"}},
	}}

	l := Merge(states, opts)

	if len(l.UnmatchedMappings) != 0 {
		t.Fatalf("mapping was not applied: %+v", l.UnmatchedMappings)
	}
	// The antenna feed is now a route for the cable channel, and no longer a
	// standalone channel of its own.
	c := find(t, l, "294")
	if len(c.Candidates) != 2 {
		t.Fatalf("channel 294 has %d candidates, want the antenna attached", len(c.Candidates))
	}
	if c.Candidates[0].Device != "antenna" {
		t.Errorf("routes to %q first, want the antenna", c.Candidates[0].Device)
	}
	for _, ch := range l.Channels {
		if ch.Number == "4.2" {
			t.Error("the mapped antenna feed should not also stand alone as 4.2")
		}
	}
}

// A mapping preferred wins over automatic name matching for the same source.
func TestMappingOverridesAutoMatch(t *testing.T) {
	states := []device.State{
		state("cable", config.SourceCable,
			ch("2", "WJBK", "MPEG2"),
			ch("294", "WDIVDT2", "MPEG2"),
		),
		// Antenna WJBK would auto-match cable 2, but the mapping sends it to 294.
		state("antenna", config.SourceAntenna, ch("2.1", "WJBK", "MPEG2")),
	}
	opts := Options{Mappings: []config.Mapping{
		{Channel: "294", Source: config.ChannelRef{Device: "antenna", Channel: "2.1"}},
	}}

	l := Merge(states, opts)

	if c := find(t, l, "294"); len(c.Candidates) != 2 {
		t.Errorf("mapping target has %d candidates, want 2", len(c.Candidates))
	}
	if c := find(t, l, "2"); len(c.Candidates) != 1 {
		t.Errorf("cable 2 has %d candidates, want the antenna diverted by the mapping", len(c.Candidates))
	}
}

// A mapping that names a missing channel or source is reported, not swallowed.
func TestUnmatchedMappingIsReported(t *testing.T) {
	states := []device.State{
		state("cable", config.SourceCable, ch("2", "WJBK", "MPEG2")),
		state("antenna", config.SourceAntenna, ch("2.1", "WJBK", "MPEG2")),
	}
	opts := Options{Mappings: []config.Mapping{
		{Channel: "999", Source: config.ChannelRef{Device: "antenna", Channel: "2.1"}}, // no channel 999
		{Channel: "2", Source: config.ChannelRef{Device: "antenna", Channel: "9.9"}},   // no source 9.9
	}}

	l := Merge(states, opts)
	if len(l.UnmatchedMappings) != 2 {
		t.Errorf("reported %d unmatched mappings, want 2", len(l.UnmatchedMappings))
	}
}

// An exclude rule drops a specific channel by device and number.
func TestExcludeDropsChannel(t *testing.T) {
	states := []device.State{
		state("cable", config.SourceCable,
			ch("2", "WJBK", "MPEG2"),
			ch("999", "INFO", "MPEG2"),
		),
	}
	opts := Options{Exclude: []config.ChannelRef{{Device: "cable", Channel: "999"}}}

	l := Merge(states, opts)
	if l.Excluded.Manual != 1 {
		t.Errorf("Excluded.Manual = %d, want 1", l.Excluded.Manual)
	}
	for _, c := range l.Channels {
		if c.Number == "999" {
			t.Error("excluded channel 999 is still present")
		}
	}
}

// find returns the merged channel presented at the given number.
func find(t *testing.T, l Lineup, number string) Channel {
	t.Helper()
	for _, c := range l.Channels {
		if c.Number == number {
			return c
		}
	}
	t.Fatalf("no channel presented at %q in %d channels", number, len(l.Channels))
	return Channel{}
}

// Channels that exist only on an alternate device still belong in the lineup.
// The antenna carries seven distinct WDWO-CD subchannels, and collapsing them
// by name would silently delete six of them.
func TestAntennaOnlyChannelsSurviveAlongsideSpine(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable, ch("2", "WJBK", "MPEG2")),
		state("antenna", config.SourceAntenna,
			ch("2.1", "WJBK", "MPEG2"), // attaches to cable 2
			ch("18.1", "WDWO-CD", "MPEG2"),
			ch("18.2", "WDWO-CD", "MPEG2"),
			ch("2.2", "Movies!", "MPEG2"),
		),
	}, Options{})

	// Cable's one channel, plus three antenna-only ones. The WJBK antenna feed
	// attaches rather than being listed separately.
	if len(l.Channels) != 4 {
		t.Fatalf("got %d channels, want 4: %+v", len(l.Channels), l.Channels)
	}
	for _, number := range []string{"18.1", "18.2", "2.2"} {
		if c := find(t, l, number); c.FromSpine() {
			t.Errorf("%s should not be marked as a spine channel", number)
		}
	}
	if c := find(t, l, "2"); len(c.Candidates) != 2 {
		t.Errorf("cable 2 got %d candidates, want the antenna attached", len(c.Candidates))
	}
}

// With no cable device configured the antenna becomes the spine, and its
// same-named subchannels must still stand apart.
func TestSameDeviceSameNameStaysSeparate(t *testing.T) {
	l := Merge([]device.State{
		state("antenna", config.SourceAntenna,
			ch("18.1", "WDWO-CD", "MPEG2"),
			ch("18.2", "WDWO-CD", "MPEG2"),
			ch("18.3", "WDWO-CD", "MPEG2"),
		),
	}, Options{})

	if len(l.Channels) != 3 {
		t.Fatalf("got %d channels, want 3 preserved", len(l.Channels))
	}
	for _, n := range []string{"18.1", "18.2", "18.3"} {
		c := find(t, l, n)
		if len(c.Candidates) != 1 {
			t.Errorf("%s should stand alone, got %d candidates", n, len(c.Candidates))
		}
	}
}

// Identity comes from the spine so guide data lines up; routing prefers the
// antenna so cable tuners are conserved. The consumer never sees the swap.
func TestSpineNamesTheChannelAndAntennaRoutesIt(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable, ch("2", "WJBK", "MPEG2")),
		state("antenna", config.SourceAntenna, ch("2.1", "WJBK", "MPEG2")),
	}, Options{})

	if len(l.Channels) != 1 {
		t.Fatalf("got %d channels, want 1 merged", len(l.Channels))
	}
	c := l.Channels[0]
	if len(c.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(c.Candidates))
	}
	if c.Number != "2" || c.Name != "WJBK" {
		t.Errorf("identity = %s %s, want cable's 2 WJBK so guide data matches", c.Number, c.Name)
	}
	if c.Candidates[0].Device != "antenna" {
		t.Errorf("first candidate is %q, want the antenna to conserve cable tuners", c.Candidates[0].Device)
	}
	if !c.FromSpine() {
		t.Error("channel should be marked as coming from the spine")
	}
}

// Cable presents the standard and high definition feeds of a station as
// separate channels with separate guide data. Sourcery must not merge them
// with each other, but the antenna feed should serve both.
func TestAntennaAttachesToBothCableListings(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable,
			ch("4", "WDIV", "MPEG2"),
			hdhr.Channel{GuideNumber: "232", GuideName: "WDIVDT", VideoCodec: "H264", AudioCodec: "AC3", HD: 1},
			ch("294", "WDIVDT2", "MPEG2"), // a different subchannel, not a twin
		),
		state("antenna", config.SourceAntenna,
			hdhr.Channel{GuideNumber: "4.1", GuideName: "WDIV-HD", VideoCodec: "MPEG2", AudioCodec: "AC3", HD: 1},
		),
	}, Options{})

	if len(l.Channels) != 3 {
		t.Fatalf("got %d channels, want cable's 3 preserved one for one", len(l.Channels))
	}
	for _, number := range []string{"4", "232"} {
		c := find(t, l, number)
		if len(c.Candidates) != 2 {
			t.Errorf("channel %s has %d candidates, want the antenna attached too",
				number, len(c.Candidates))
			continue
		}
		if c.Candidates[0].Device != "antenna" {
			t.Errorf("channel %s routes to %q first, want the antenna", number, c.Candidates[0].Device)
		}
	}
	// WDIVDT2 is a different programme and must not pick up the antenna feed.
	if c := find(t, l, "294"); len(c.Candidates) != 1 {
		t.Errorf("WDIVDT2 got %d candidates, want only its own", len(c.Candidates))
	}
}

// WUDT is a real callsign; trimming DT from it would leave a two-letter stem
// that could collide with something unrelated.
func TestDigitalSuffixTrimmingIsGuarded(t *testing.T) {
	if got := normalizeName("WDIVDT"); got != "WDIV" {
		t.Errorf("normalizeName(WDIVDT) = %q, want WDIV", got)
	}
	if got := normalizeName("WUDT-LD"); got != "WUDT" {
		t.Errorf("normalizeName(WUDT-LD) = %q, want WUDT left intact", got)
	}
	if got := normalizeName("WDIVDT2"); got != "WDIVDT2" {
		t.Errorf("normalizeName(WDIVDT2) = %q, want the subchannel left intact", got)
	}
}

// The antenna suffixes callsigns with a transmission type; cable does not.
func TestBroadcastSuffixIsStripped(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable, ch("4", "WDIV", "MPEG2")),
		state("antenna", config.SourceAntenna, ch("4.1", "WDIV-HD", "MPEG2")),
	}, Options{})
	if len(l.Channels) != 1 {
		t.Fatalf("WDIV-HD and WDIV should merge, got %d channels", len(l.Channels))
	}

	for _, name := range []string{"CHWI_HD", "WDWO-CD", "WUDT-LD", "WWJ-HD"} {
		if got, want := normalizeName(name), normalizeName(trimForTest(name)); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", name, got, want)
		}
	}
}

func trimForTest(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' || s[i] == '_' {
			return s[:i]
		}
	}
	return s
}

// Cable names its own HD variants without a separator, and there they are
// genuinely different channels.
func TestSuffixOnlyStrippedWithSeparator(t *testing.T) {
	if normalizeName("QVCHD") == normalizeName("QVC") {
		t.Error("QVCHD and QVC must not collapse; cable carries both as distinct channels")
	}
	if normalizeName("WORD") != "WORD" {
		t.Errorf("normalizeName(WORD) = %q, want WORD left intact", normalizeName("WORD"))
	}
	if normalizeName("Movies!") != "MOVIES" {
		t.Errorf("punctuation should be stripped, got %q", normalizeName("Movies!"))
	}
}

// When ATSC 3.0 is admitted, its HEVC streams must still rank below a playable
// twin: a stream that will not play is worth nothing, however cheap its tuner.
func TestCompatibleCodecOutranksSourcePreference(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable, ch("3", "WMYD", "MPEG2")),
		state("antenna", config.SourceAntenna, ch("120.1", "WMYD", "HEVC")),
	}, Options{AllowATSC3: true})

	c := l.Channels[0]
	if c.Candidates[0].VideoCodec != "MPEG2" {
		t.Errorf("first candidate is %s, want the playable MPEG2 stream",
			c.Candidates[0].VideoCodec)
	}
	if c.Candidates[0].Device != "cable" {
		t.Errorf("first candidate device = %q, want prime despite the antenna preference",
			c.Candidates[0].Device)
	}
}

// Cable carries copy-protected MPEG2 channels, so DRM exclusion has to work
// independently of the codec.
// Picture quality outranks tuner economy: a viewer notices standard
// definition, and will not notice which tuner produced the picture.
func TestHDOutranksSourcePreference(t *testing.T) {
	hd := func(number, name string) hdhr.Channel {
		return hdhr.Channel{GuideNumber: number, GuideName: name, VideoCodec: "MPEG2", AudioCodec: "AC3", HD: 1}
	}

	// Antenna is standard definition here, cable is high definition.
	l := Merge([]device.State{
		state("cable", config.SourceCable, hd("7", "WXYZ")),
		state("antenna", config.SourceAntenna, ch("7.1", "WXYZ-HD", "MPEG2")),
	}, Options{})

	c := l.Channels[0]
	if c.Candidates[0].Device != "cable" {
		t.Errorf("routed to %q first, want the high definition cable feed",
			c.Candidates[0].Device)
	}

	// With both high definition, the antenna wins on tuner economy.
	l = Merge([]device.State{
		state("cable", config.SourceCable, hd("7", "WXYZ")),
		state("antenna", config.SourceAntenna, hd("7.1", "WXYZ-HD")),
	}, Options{})

	if got := l.Channels[0].Candidates[0].Device; got != "antenna" {
		t.Errorf("routed to %q first, want flex once quality ties", got)
	}
}

func TestProtectedChannelsExcluded(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable,
			ch("2", "WJBK", "MPEG2"),
			hdhr.Channel{GuideNumber: "999", GuideName: "LOCKED", VideoCodec: "MPEG2", DRM: 1},
		),
	}, Options{})

	if l.Excluded.DRM != 1 {
		t.Errorf("Excluded.DRM = %d, want 1", l.Excluded.DRM)
	}
	if len(l.Channels) != 1 {
		t.Fatalf("got %d channels, want the DRM entry dropped", len(l.Channels))
	}
	if l.Channels[0].Number != "2" {
		t.Errorf("kept %q, want the unprotected channel", l.Channels[0].Number)
	}
}

// ATSC 3.0 is dropped wholesale by default: its AC4 audio does not play
// reliably, and every such channel here shadows an ATSC 1.0 twin that does.
func TestATSC3ExcludedByDefault(t *testing.T) {
	states := []device.State{
		state("antenna", config.SourceAntenna,
			ch("2.1", "WJBK", "MPEG2"),
			hdhr.Channel{GuideNumber: "102.1", GuideName: "WJBK", VideoCodec: "HEVC", AudioCodec: "AC4"},
			hdhr.Channel{GuideNumber: "104.1", GuideName: "WDIV", VideoCodec: "HEVC", AudioCodec: "AC4"},
		),
	}

	l := Merge(states, Options{})
	if l.Excluded.ATSC3 != 2 {
		t.Errorf("Excluded.ATSC3 = %d, want 2", l.Excluded.ATSC3)
	}
	if len(l.Channels) != 1 || l.Channels[0].Number != "2.1" {
		t.Fatalf("want only the ATSC 1.0 channel, got %+v", l.Channels)
	}

	// The capability is retained, just off.
	if allowed := Merge(states, Options{AllowATSC3: true}); allowed.Excluded.ATSC3 != 0 {
		t.Errorf("AllowATSC3 should admit them, still excluded %d", allowed.Excluded.ATSC3)
	}
}

// H264 is ordinary: cable carries 184 such channels. Treating it as
// next-generation would exclude a third of the lineup.
func TestH264IsNotTreatedAsATSC3(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable, ch("36", "TRUTV", "H264")),
	}, Options{})

	if l.Excluded.ATSC3 != 0 {
		t.Fatalf("H264 was excluded as ATSC 3.0")
	}
	if !compatible("H264") {
		t.Error("H264 must rank as playable")
	}
}

// ATSC 3.0 companion feeds for handheld devices are never what a DVR wants.
// Checked with ATSC 3.0 admitted, since otherwise the wholesale exclusion
// would mask whether this rule works at all.
func TestMobileVariantsExcluded(t *testing.T) {
	l := Merge([]device.State{
		state("antenna", config.SourceAntenna,
			ch("7.1", "WXYZ-HD", "MPEG2"),
			ch("107.99", "WXYZMOB", "HEVC"), // .99 subchannel and MOB callsign
			ch("120.99", "WMYDMOB", "HEVC"),
		),
	}, Options{AllowATSC3: true})

	if l.Excluded.Mobile != 2 {
		t.Errorf("Excluded.Mobile = %d, want 2", l.Excluded.Mobile)
	}
	if len(l.Channels) != 1 || l.Channels[0].Number != "7.1" {
		t.Fatalf("want only the television feed to survive, got %+v", l.Channels)
	}
}

// Either signal alone is enough, since broadcasters are not consistent.
func TestMobileDetection(t *testing.T) {
	tests := []struct {
		number, name string
		want         bool
	}{
		{"107.99", "WXYZMOB", true},    // both signals
		{"120.99", "WMYD-HD", true},    // .99 alone
		{"31.1", "SOMETHINGMOB", true}, // MOB suffix alone
		{"7.1", "WXYZ-HD", false},
		{"9.9", "WWJ", false}, // .9 is a normal subchannel
	}

	for _, tt := range tests {
		got := mobileVariant(hdhr.Channel{GuideNumber: tt.number, GuideName: tt.name})
		if got != tt.want {
			t.Errorf("mobileVariant(%s %s) = %v, want %v", tt.number, tt.name, got, tt.want)
		}
	}
}

func TestUnreachableDeviceContributesNothing(t *testing.T) {
	broken := state("cable", config.SourceCable, ch("2", "WJBK", "MPEG2"))
	broken.Err = errors.New("probe failed")

	l := Merge([]device.State{
		broken,
		state("antenna", config.SourceAntenna, ch("2.1", "WJBK", "MPEG2")),
	}, Options{})
	if len(l.Channels) != 1 || len(l.Channels[0].Candidates) != 1 {
		t.Fatalf("a failed probe must not contribute channels: %+v", l.Channels)
	}
	if l.Channels[0].Candidates[0].Device != "antenna" {
		t.Error("only the reachable device should appear")
	}
}

// Viewers expect 9 before 10, and 2.2 before 2.10.
func TestChannelOrdering(t *testing.T) {
	l := Merge([]device.State{
		state("cable", config.SourceCable,
			ch("10", "TEN", "MPEG2"),
			ch("9", "NINE", "MPEG2"),
			ch("2.10", "B", "MPEG2"),
			ch("2.2", "A", "MPEG2"),
		),
	}, Options{})

	var got []string
	for _, c := range l.Channels {
		got = append(got, c.Number)
	}
	want := []string{"2.2", "2.10", "9", "10"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestAsHDHomeRunPointsAtSourcery(t *testing.T) {
	l := Merge([]device.State{
		state("antenna", config.SourceAntenna, hdhr.Channel{
			GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", AudioCodec: "AC3", HD: 1,
			URL: "http://192.0.2.10:5004/auto/v2.1",
		}),
	}, Options{})

	out := l.AsHDHomeRun("http://192.0.2.20:5004")
	if len(out) != 1 {
		t.Fatalf("got %d entries, want 1", len(out))
	}
	if out[0].URL != "http://192.0.2.20:5004/auto/v2.1" {
		t.Errorf("URL = %q, want it pointing at Sourcery rather than the device", out[0].URL)
	}
	if out[0].HD != 1 {
		t.Errorf("HD = %d, want 1 carried through", out[0].HD)
	}
}
