package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sourcery/internal/arbiter"
	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
	"sourcery/internal/lineup"
	"sourcery/internal/stream"
)

func testServer(t *testing.T, chans ...hdhr.Channel) *Server {
	t.Helper()
	return testServerWithTuners(t, 4, chans...)
}

// testServerWithTuners builds a server backed by a single antenna device with
// the given tuner count, so capacity exhaustion can be exercised.
func testServerWithTuners(t *testing.T, tuners int, chans ...hdhr.Channel) *Server {
	t.Helper()
	cfg := &config.Config{
		Listen:       ":5004",
		FriendlyName: "Sourcery",
		TunerCount:   7,
		Devices: []config.Device{
			{Name: "antenna", Address: "192.0.2.10", Source: config.SourceAntenna},
		},
	}
	states := []device.State{{
		Device:   &device.Device{Device: cfg.Devices[0]},
		Discover: &hdhr.Discover{TunerCount: tuners},
		Lineup:   chans,
	}}

	s, err := New(cfg, arbiter.New(states), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetLineup(lineup.Merge(states, lineup.Options{AllowATSC3: cfg.AllowATSC3}))
	return s
}

// fakeTuner serves a transport stream of the given size, and reports how many
// concurrent connections it saw.
func fakeTuner(t *testing.T, payload []byte) (url string, opened *atomic.Int32) {
	t.Helper()
	opened = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		opened.Add(1)
		w.Header().Set("Content-Type", stream.ContentType)
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, opened
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

// The "v" prefix means "tune by virtual channel number" and is not part of the
// number, so it must be stripped before the lineup is consulted.
func TestStreamRelaysUpstream(t *testing.T) {
	payload := bytes.Repeat([]byte{0x47, 0x01, 0x02, 0x03}, 5000)
	url, opened := fakeTuner(t, payload)

	s := testServer(t, hdhr.Channel{
		GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", URL: url,
	})

	resp := get(t, s, "/auto/v2.1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != stream.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, stream.ContentType)
	}

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Errorf("relayed %d bytes, want the %d sent upstream", len(got), len(payload))
	}
	if n := opened.Load(); n != 1 {
		t.Errorf("opened %d upstream connections, want 1", n)
	}
}

func TestUnknownChannelIsNotFound(t *testing.T) {
	s := testServer(t, hdhr.Channel{GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2"})
	if resp := get(t, s, "/auto/v99.9"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404", resp.Status)
	}
}

// The tuner is returned when the stream ends, or a device would be left
// permanently "full" from the arbiter's point of view.
func TestTunerIsReleasedAfterStreaming(t *testing.T) {
	url, _ := fakeTuner(t, []byte("stream"))
	s := testServerWithTuners(t, 1, hdhr.Channel{
		GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", URL: url,
	})

	for i := range 3 {
		resp := get(t, s, "/auto/v2.1")
		io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %s, want 200 on a released tuner", i+1, resp.Status)
		}
	}
	if held := s.arbiter.Snapshot()[0].Held; held != 0 {
		t.Errorf("%d tuners still held after every stream ended", held)
	}
}

// holdOpen serves a stream that stays open until the returned func is called,
// so a tuner can be kept occupied for the duration of a test.
func holdOpen(t *testing.T) (url string, release func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
		http.NewResponseController(w).Flush()
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() { once.Do(func() { close(done) }) }
}

// streamInBackground starts a cancellable streaming request and returns a func
// to end it. Streams block until the consumer disconnects, so tests drive them
// this way and assert on the server's own accounting rather than on a response
// body that will not complete.
func streamInBackground(t *testing.T, s *Server, path string) (cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil).WithContext(ctx)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

// waitFor polls until cond holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func heldOn(s *Server, device string) int {
	for _, st := range s.arbiter.Snapshot() {
		if st.Device == device {
			return st.Held
		}
	}
	return -1
}

// Never tear down a stream in flight to make room for a new one; refuse
// instead. A different channel on a single-tuner device has nowhere to go.
func TestRefusesWhenNoTunerIsFree(t *testing.T) {
	url, release := holdOpen(t)
	defer release()

	s := testServerWithTuners(t, 1,
		hdhr.Channel{GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", URL: url},
		hdhr.Channel{GuideNumber: "5.1", GuideName: "WKAR", VideoCodec: "MPEG2", URL: url + "/other"},
	)

	streamInBackground(t, s, "/auto/v2.1")
	waitFor(t, "the first stream to take the tuner", func() bool { return heldOn(s, "antenna") == 1 })

	if resp := get(t, s, "/auto/v5.1"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %s, want 503 when the only tuner is busy", resp.Status)
	}
}

// The heart of M3: a second consumer of a channel already being received joins
// the existing upstream instead of taking another tuner.
func TestSecondConsumerReusesTheTuner(t *testing.T) {
	url, release := holdOpen(t)
	defer release()

	s := testServerWithTuners(t, 1, hdhr.Channel{
		GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", URL: url,
	})

	// Two consumers of the same channel, on a device with a single tuner.
	streamInBackground(t, s, "/auto/v2.1")
	waitFor(t, "one subscriber", func() bool { return subscribers(s) == 1 })
	streamInBackground(t, s, "/auto/v2.1")
	waitFor(t, "the second consumer to join", func() bool { return subscribers(s) == 2 })

	// The second joined rather than failing, and both share one tuner.
	if held := heldOn(s, "antenna"); held != 1 {
		t.Errorf("%d tuners held, want exactly 1 shared between both consumers", held)
	}
	live := s.hub.Snapshot()
	if len(live) != 1 || live[0].Subscribers != 2 {
		t.Errorf("hub snapshot = %+v, want one stream with 2 subscribers", live)
	}
}

func subscribers(s *Server) int {
	var n int
	for _, st := range s.hub.Snapshot() {
		n += st.Subscribers
	}
	return n
}

// The arbiter's view of foreign usage is always slightly stale, so a device may
// refuse a tuner it was believed to have. The next candidate must be tried.
func TestFallsBackWhenUpstreamRefuses(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no free tuner", http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	good, opened := fakeTuner(t, []byte("picture"))

	cfg := &config.Config{
		FriendlyName: "Sourcery", TunerCount: 7,
		Devices: []config.Device{
			{Name: "antenna", Address: "1.1.1.1", Source: config.SourceAntenna},
			{Name: "cable", Address: "2.2.2.2", Source: config.SourceCable},
		},
	}
	states := []device.State{
		{ // preferred, but will refuse
			Device:   &device.Device{Device: cfg.Devices[0]},
			Discover: &hdhr.Discover{TunerCount: 4},
			Lineup: []hdhr.Channel{{
				GuideNumber: "2.1", GuideName: "WJBK", VideoCodec: "MPEG2", URL: busy.URL,
			}},
		},
		{
			Device:   &device.Device{Device: cfg.Devices[1]},
			Discover: &hdhr.Discover{TunerCount: 3},
			Lineup: []hdhr.Channel{{
				GuideNumber: "2", GuideName: "WJBK", VideoCodec: "MPEG2", URL: good,
			}},
		},
	}

	arb := arbiter.New(states)
	s, err := New(cfg, arb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetLineup(lineup.Merge(states, lineup.Options{}))

	resp := get(t, s, "/auto/v2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200 after falling back", resp.Status)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "picture" {
		t.Errorf("body = %q, want the fallback device's stream", body)
	}
	if opened.Load() != 1 {
		t.Error("the fallback device was not used")
	}
	// The failed attempt must not leak a lease.
	for _, st := range arb.Snapshot() {
		if st.Held != 0 {
			t.Errorf("%s still holds %d tuners after the stream ended", st.Device, st.Held)
		}
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
		Devices:  []config.Device{{Name: "antenna", Address: "1.2.3.4", Source: config.SourceAntenna}},
	}
	s, err := New(cfg, arbiter.New(nil), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.DeviceID() != "1234ABC2" {
		t.Errorf("DeviceID = %q, want the configured value", s.DeviceID())
	}

	cfg.DeviceID = "1234ABC3" // fails the checksum
	if _, err := New(cfg, arbiter.New(nil), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("an invalid configured device id should be rejected at startup")
	}
}
