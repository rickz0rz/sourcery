package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
	"sourcery/internal/lineup"
)

func testServer(t *testing.T, chans ...hdhr.Channel) *Server {
	t.Helper()
	cfg := &config.Config{
		Listen:       ":5004",
		FriendlyName: "Sourcery",
		TunerCount:   7,
		Devices: []config.Device{
			{Name: "flex", Address: "192.0.2.10", Source: config.SourceAntenna},
		},
	}
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetLineup(lineup.Merge([]device.State{{
		Device: &device.Device{Device: cfg.Devices[0]},
		Lineup: chans,
	}}))
	return s
}

func get(t *testing.T, s *Server, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.20:5004"+path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// Consumers reject a device whose ID fails the HDHomeRun checksum, so the
// emulated identity has to be well-formed.
func TestDiscoverAdvertisesValidIdentity(t *testing.T) {
	s := testServer(t)
	resp := get(t, s, "/discover.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}

	var d hdhr.Discover
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := hdhr.ParseDeviceID(d.DeviceID); err != nil {
		t.Errorf("advertised device id %q is not valid: %v", d.DeviceID, err)
	}
	if d.TunerCount != 7 {
		t.Errorf("TunerCount = %d, want 7", d.TunerCount)
	}
	if d.FriendlyName != "Sourcery" {
		t.Errorf("FriendlyName = %q", d.FriendlyName)
	}
	// The base URL must reflect the host the consumer used to reach us.
	if d.BaseURL != "http://192.0.2.20:5004" {
		t.Errorf("BaseURL = %q, want it derived from the request host", d.BaseURL)
	}
	if d.DeviceAuth != "" {
		t.Error("must not advertise a DeviceAuth token")
	}
}

func TestDiscoverHonoursAdvertiseAddress(t *testing.T) {
	s := testServer(t)
	s.cfg.AdvertiseAddress = "sourcery.local:5004"

	var d hdhr.Discover
	json.NewDecoder(get(t, s, "/discover.json").Body).Decode(&d)
	if d.BaseURL != "http://sourcery.local:5004" {
		t.Errorf("BaseURL = %q, want the configured override", d.BaseURL)
	}
}

func TestLineupPointsBackAtSourcery(t *testing.T) {
	s := testServer(t,
		hdhr.Channel{GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", HD: 1},
	)

	var got []hdhr.Channel
	if err := json.NewDecoder(get(t, s, "/lineup.json").Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d channels, want 1", len(got))
	}
	if got[0].URL != "http://192.0.2.20:5004/auto/v2.1" {
		t.Errorf("URL = %q, want it pointing at Sourcery", got[0].URL)
	}
}

// A nil slice would marshal as null, which consumers do not accept in place of
// an empty lineup.
func TestEmptyLineupMarshalsAsArray(t *testing.T) {
	s := testServer(t)
	body, _ := io.ReadAll(get(t, s, "/lineup.json").Body)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

// Sourcery derives its lineup rather than scanning, so a consumer waiting for a
// scan to finish must see one that is already complete.
func TestLineupStatusReportsNoScan(t *testing.T) {
	var ls hdhr.LineupStatus
	json.NewDecoder(get(t, testServer(t), "/lineup_status.json").Body).Decode(&ls)
	if ls.ScanInProgress != 0 {
		t.Errorf("ScanInProgress = %d, want 0", ls.ScanInProgress)
	}
}

// Some consumers trigger a scan during setup and treat failure as fatal.
func TestLineupPostSucceeds(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://x/lineup.post?scan=start", nil)
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestDeviceXML(t *testing.T) {
	resp := get(t, testServer(t), "/device.xml")
	body, _ := io.ReadAll(resp.Body)

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"<friendlyName>Sourcery</friendlyName>", "urn:schemas-upnp-org"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("device.xml missing %q", want)
		}
	}
}

// Until M2 lands, a stream request must fail fast rather than hang. The "v"
// prefix means "tune by virtual channel number" and is not part of the number,
// so it must be stripped before the lineup is consulted.
func TestStreamResolvesChannelThenRefuses(t *testing.T) {
	s := testServer(t, hdhr.Channel{GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2"})

	if resp := get(t, s, "/auto/v2.1"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("known channel: status = %s, want 503", resp.Status)
	}
	// An unknown channel is a different failure from an unimplemented one.
	if resp := get(t, s, "/auto/v99.9"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown channel: status = %s, want 404", resp.Status)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	if resp := get(t, testServer(t), "/nonsense"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404", resp.Status)
	}
}

func TestConfiguredDeviceIDIsUsed(t *testing.T) {
	cfg := &config.Config{
		FriendlyName: "Sourcery", TunerCount: 7,
		DeviceID: "1234ABC2",
		Devices:  []config.Device{{Name: "flex", Address: "1.2.3.4", Source: config.SourceAntenna}},
	}
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.DeviceID() != "1234ABC2" {
		t.Errorf("DeviceID = %q, want the configured value", s.DeviceID())
	}

	cfg.DeviceID = "1234ABC3" // fails the checksum
	if _, err := New(cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("an invalid configured device id should be rejected at startup")
	}
}
