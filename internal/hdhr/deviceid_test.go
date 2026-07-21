package hdhr

import "testing"

// The first two IDs below were captured from working hardware, so they are
// known good. They are the ground truth that the checksum algorithm is right:
// both must validate, and a single-nibble corruption of each must not.
func TestValidDeviceIDMatchesKnownGoodIDs(t *testing.T) {
	tests := []struct {
		id   uint32
		want bool
	}{
		{0x1234ABC2, true},  // captured from a CableCARD tuner
		{0xABCDEF01, true},  // captured from an ATSC tuner
		{0x1234ABC3, false}, // the first, low nibble corrupted
		{0xABCDEF00, false}, // the second, low nibble corrupted
		{0x12345678, false},
		{0x00000000, false}, // reserved: unset
		{0xFFFFFFFF, false}, // reserved: wildcard
	}

	for _, tt := range tests {
		if got := ValidDeviceID(tt.id); got != tt.want {
			t.Errorf("ValidDeviceID(%08X) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestParseDeviceID(t *testing.T) {
	got, err := ParseDeviceID("1234ABC2")
	if err != nil {
		t.Fatalf("ParseDeviceID: %v", err)
	}
	if got != 0x1234ABC2 {
		t.Errorf("got %08X, want 1234ABC2", got)
	}
	if FormatDeviceID(got) != "1234ABC2" {
		t.Errorf("FormatDeviceID round trip = %q", FormatDeviceID(got))
	}

	for _, bad := range []string{"1234ABC3", "nothex", "", "1234ABC212"} {
		if _, err := ParseDeviceID(bad); err == nil {
			t.Errorf("ParseDeviceID(%q) should have failed", bad)
		}
	}
}

// Consumers key their tuner configuration off the device ID, so it must be
// valid, stable across runs, and independent of device ordering.
func TestDeriveDeviceID(t *testing.T) {
	seeds := []string{"192.0.2.10", "192.0.2.11"}

	id := DeriveDeviceID(seeds)
	if !ValidDeviceID(id) {
		t.Fatalf("DeriveDeviceID produced invalid id %08X", id)
	}
	if again := DeriveDeviceID(seeds); again != id {
		t.Errorf("not stable: %08X then %08X", id, again)
	}
	reordered := DeriveDeviceID([]string{"192.0.2.11", "192.0.2.10"})
	if reordered != id {
		t.Errorf("order dependent: %08X vs %08X", id, reordered)
	}
	if other := DeriveDeviceID([]string{"198.51.100.5"}); other == id {
		t.Error("different fleets should get different ids")
	}
}

// fixChecksum must never emit a reserved value. 0xFFFFFFF0 is the input that
// would otherwise fix up to the wildcard.
func TestDeriveDeviceIDAvoidsReserved(t *testing.T) {
	if got := fixChecksum(0xFFFFFFF0); got != 0xFFFFFFFF {
		t.Fatalf("expected the collision case to produce the wildcard, got %08X", got)
	}
	if ValidDeviceID(0xFFFFFFFF) {
		t.Error("wildcard must be rejected so the fallback path is reachable")
	}
}
