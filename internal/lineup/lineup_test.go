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

// The antenna carries seven distinct WDWO-CD subchannels. Collapsing them by
// name would silently delete six channels from the lineup.
func TestSameDeviceSameNameStaysSeparate(t *testing.T) {
	l := Merge([]device.State{
		state("flex", config.SourceAntenna,
			ch("18.1", "WDWO-CD", "MPEG2"),
			ch("18.2", "WDWO-CD", "MPEG2"),
			ch("18.3", "WDWO-CD", "MPEG2"),
		),
	})

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

func TestCrossDeviceMergesAndPrefersAntenna(t *testing.T) {
	l := Merge([]device.State{
		state("prime", config.SourceCable, ch("2", "WJBK", "MPEG2")),
		state("flex", config.SourceAntenna, ch("2.1", "WJBK", "MPEG2")),
	})

	if len(l.Channels) != 1 {
		t.Fatalf("got %d channels, want 1 merged", len(l.Channels))
	}
	c := l.Channels[0]
	if len(c.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(c.Candidates))
	}
	if c.Candidates[0].Device != "flex" {
		t.Errorf("first candidate is %q, want the antenna to conserve cable tuners", c.Candidates[0].Device)
	}
	if c.Number != "2.1" {
		t.Errorf("presented number = %q, want the preferred candidate's 2.1", c.Number)
	}
}

// The antenna suffixes callsigns with a transmission type; cable does not.
func TestBroadcastSuffixIsStripped(t *testing.T) {
	l := Merge([]device.State{
		state("prime", config.SourceCable, ch("4", "WDIV", "MPEG2")),
		state("flex", config.SourceAntenna, ch("4.1", "WDIV-HD", "MPEG2")),
	})
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

// ATSC 3.0 twins are HEVC/AC4, which the consumers handle poorly. A playable
// stream matters more than which tuner it costs.
func TestCompatibleCodecOutranksSourcePreference(t *testing.T) {
	l := Merge([]device.State{
		state("prime", config.SourceCable, ch("3", "WMYD", "MPEG2")),
		state("flex", config.SourceAntenna, ch("120.1", "WMYD", "HEVC")),
	})

	c := l.Channels[0]
	if c.Candidates[0].VideoCodec != "MPEG2" {
		t.Errorf("first candidate is %s, want the playable MPEG2 stream",
			c.Candidates[0].VideoCodec)
	}
	if c.Candidates[0].Device != "prime" {
		t.Errorf("first candidate device = %q, want prime despite the antenna preference",
			c.Candidates[0].Device)
	}
}

func TestProtectedChannelsExcluded(t *testing.T) {
	l := Merge([]device.State{
		state("flex", config.SourceAntenna,
			ch("2.1", "WJBK", "MPEG2"),
			hdhr.Channel{GuideNumber: "102.1", GuideName: "WJBK", VideoCodec: "HEVC", DRM: 1},
		),
	})

	if l.Excluded.DRM != 1 {
		t.Errorf("Excluded.DRM = %d, want 1", l.Excluded.DRM)
	}
	if len(l.Channels) != 1 {
		t.Fatalf("got %d channels, want the DRM entry dropped", len(l.Channels))
	}
	if l.Channels[0].Number != "2.1" {
		t.Errorf("kept %q, want the unprotected 2.1", l.Channels[0].Number)
	}
}

// ATSC 3.0 companion feeds for handheld devices are never what a DVR wants.
func TestMobileVariantsExcluded(t *testing.T) {
	l := Merge([]device.State{
		state("flex", config.SourceAntenna,
			ch("7.1", "WXYZ-HD", "MPEG2"),
			ch("107.99", "WXYZMOB", "HEVC"), // .99 subchannel and MOB callsign
			ch("120.99", "WMYDMOB", "HEVC"),
		),
	})

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
	broken := state("prime", config.SourceCable, ch("2", "WJBK", "MPEG2"))
	broken.Err = errors.New("probe failed")

	l := Merge([]device.State{
		broken,
		state("flex", config.SourceAntenna, ch("2.1", "WJBK", "MPEG2")),
	})
	if len(l.Channels) != 1 || len(l.Channels[0].Candidates) != 1 {
		t.Fatalf("a failed probe must not contribute channels: %+v", l.Channels)
	}
	if l.Channels[0].Candidates[0].Device != "flex" {
		t.Error("only the reachable device should appear")
	}
}

// Viewers expect 9 before 10, and 2.2 before 2.10.
func TestChannelOrdering(t *testing.T) {
	l := Merge([]device.State{
		state("prime", config.SourceCable,
			ch("10", "TEN", "MPEG2"),
			ch("9", "NINE", "MPEG2"),
			ch("2.10", "B", "MPEG2"),
			ch("2.2", "A", "MPEG2"),
		),
	})

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
		state("flex", config.SourceAntenna, hdhr.Channel{
			GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", AudioCodec: "AC3", HD: 1,
			URL: "http://192.0.2.10:5004/auto/v2.1",
		}),
	})

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
