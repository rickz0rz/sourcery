package hdhr

import (
	"encoding/json"
	"testing"
)

// Fixtures below are real device responses, trimmed to a few entries, with
// DeviceAuth redacted and addresses replaced with documentation ones. Keep new
// fixtures real: the field-presence quirks they capture are the whole reason
// these tests exist.

const discoverCableCard = `{"FriendlyName":"HDHomeRun PRIME","ModelNumber":"HDHR3-CC","FirmwareName":"hdhomerun3_cablecard","FirmwareVersion":"20260313","DeviceID":"1234ABC2","DeviceAuth":"redacted","BaseURL":"http://192.0.2.11","LineupURL":"http://192.0.2.11/lineup.json","TunerCount":3,"ConditionalAccess":1}`

func TestDiscoverParsesCableCARDDevice(t *testing.T) {
	var d Discover
	if err := json.Unmarshal([]byte(discoverCableCard), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.TunerCount != 3 {
		t.Errorf("TunerCount = %d, want 3", d.TunerCount)
	}
	if d.ModelNumber != "HDHR3-CC" {
		t.Errorf("ModelNumber = %q, want HDHR3-CC", d.ModelNumber)
	}
	if d.ConditionalAccess != 1 {
		t.Errorf("ConditionalAccess = %d, want 1 (CableCARD present)", d.ConditionalAccess)
	}
}

// The antenna lineup carries signal fields and HD flags that the cable lineup
// omits, and both carry DRM only on protected entries. Absent must decode as
// zero rather than failing.
const lineupMixed = `[
 {"GuideNumber":"2.1","GuideName":"WJBK","VideoCodec":"MPEG2","AudioCodec":"AC3","HD":1,"SignalStrength":93,"SignalQuality":75,"URL":"http://192.0.2.10:5004/auto/v2.1"},
 {"GuideNumber":"2.2","GuideName":"Movies!","VideoCodec":"MPEG2","AudioCodec":"AC3","SignalStrength":93,"SignalQuality":75,"URL":"http://192.0.2.10:5004/auto/v2.2"},
 {"GuideNumber":"4","GuideName":"WDIV","VideoCodec":"MPEG2","AudioCodec":"AC3","URL":"http://192.0.2.11:5004/auto/v4"},
 {"GuideNumber":"999","GuideName":"LOCKED","VideoCodec":"MPEG2","AudioCodec":"AC3","DRM":1,"URL":"http://192.0.2.11:5004/auto/v999"}
]`

func TestLineupParsesOptionalFields(t *testing.T) {
	var ch []Channel
	if err := json.Unmarshal([]byte(lineupMixed), &ch); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(ch) != 4 {
		t.Fatalf("got %d channels, want 4", len(ch))
	}

	if ch[0].HD != 1 || ch[0].SignalStrength != 93 {
		t.Errorf("antenna entry lost optional fields: %+v", ch[0])
	}
	// A cable entry reports no signal or HD data at all.
	if ch[2].HD != 0 || ch[2].SignalStrength != 0 {
		t.Errorf("absent fields should decode as zero: %+v", ch[2])
	}

	if ch[0].Protected() {
		t.Error("channel without DRM reported as protected")
	}
	if !ch[3].Protected() {
		t.Error("channel with DRM=1 not reported as protected")
	}
}

// An idle tuner reports only Resource. This is the sole distinction between
// "idle" and "in use", so it has to survive decoding intact.
const statusPartlyBusy = `[
 {"Resource":"tuner0","VctNumber":"232","VctName":"WDIVDT","Frequency":267000000,"SignalStrengthPercent":95,"SignalQualityPercent":100,"SymbolQualityPercent":100,"TargetIP":"192.0.2.50"},
 {"Resource":"tuner1"},
 {"Resource":"tuner2"}
]`

func TestStatusDistinguishesIdleFromActive(t *testing.T) {
	var tuners []Tuner
	if err := json.Unmarshal([]byte(statusPartlyBusy), &tuners); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tuners) != 3 {
		t.Fatalf("got %d tuners, want 3", len(tuners))
	}

	if !tuners[0].Active() {
		t.Error("tuner with a frequency should be active")
	}
	if tuners[0].TargetIP != "192.0.2.50" {
		t.Errorf("TargetIP = %q, want 192.0.2.50", tuners[0].TargetIP)
	}
	for _, tn := range tuners[1:] {
		if tn.Active() {
			t.Errorf("%s reports only Resource and should be idle", tn.Resource)
		}
	}
}

func TestLineupStatusParsesSourceList(t *testing.T) {
	const raw = `{"ScanInProgress":0,"ScanPossible":1,"Source":"Antenna","SourceList":["Antenna","Cable"]}`

	var s LineupStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Source != "Antenna" {
		t.Errorf("Source = %q, want Antenna", s.Source)
	}
	if len(s.SourceList) != 2 {
		t.Errorf("SourceList = %v, want 2 entries", s.SourceList)
	}
}
