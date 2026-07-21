package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sourcery/internal/config"
	"sourcery/internal/hdhr"
)

func TestStateAccounting(t *testing.T) {
	s := State{
		Discover: &hdhr.Discover{TunerCount: 3},
		Tuners: []hdhr.Tuner{
			{Resource: "tuner0", Frequency: 267000000, TargetIP: "192.0.2.50"},
			{Resource: "tuner1"},
			{Resource: "tuner2"},
		},
		Lineup: []hdhr.Channel{
			{GuideNumber: "2"},
			{GuideNumber: "999", DRM: 1},
		},
	}

	if got := s.InUse(); got != 1 {
		t.Errorf("InUse() = %d, want 1", got)
	}
	if got := s.Free(); got != 2 {
		t.Errorf("Free() = %d, want 2", got)
	}
	if got := s.Protected(); got != 1 {
		t.Errorf("Protected() = %d, want 1", got)
	}
}

// A device that never answered has no tuner count, so it must contribute no
// capacity rather than appearing to have some.
func TestUnreachableDeviceHasNoCapacity(t *testing.T) {
	s := State{Err: context.DeadlineExceeded}
	if got := s.Free(); got != 0 {
		t.Errorf("Free() = %d, want 0 for an unreachable device", got)
	}
}

func fakeDevice(t *testing.T, tunerCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/discover.json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(hdhr.Discover{ModelNumber: "FAKE", TunerCount: tunerCount})
	})
	mux.HandleFunc("/lineup.json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]hdhr.Channel{{GuideNumber: "2", GuideName: "WJBK"}})
	})
	mux.HandleFunc("/status.json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]hdhr.Tuner{{Resource: "tuner0"}})
	})
	mux.HandleFunc("/lineup_status.json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(hdhr.LineupStatus{Source: "Antenna"})
	})
	return httptest.NewServer(mux)
}

func TestProbeCollectsAllEndpoints(t *testing.T) {
	srv := fakeDevice(t, 4)
	defer srv.Close()

	r := New(&config.Config{Devices: []config.Device{
		{Name: "antenna", Address: strings.TrimPrefix(srv.URL, "http://"), Source: config.SourceAntenna},
	}})

	states := r.Probe(context.Background())
	if len(states) != 1 {
		t.Fatalf("got %d states, want 1", len(states))
	}
	s := states[0]
	if s.Err != nil {
		t.Fatalf("probe: %v", s.Err)
	}
	if s.Discover.TunerCount != 4 || len(s.Lineup) != 1 || len(s.Tuners) != 1 || s.ScanStatus == nil {
		t.Errorf("probe returned an incomplete state: %+v", s)
	}
}

// One dead device must not take down the fleet, and results must stay in
// configuration order regardless of which device answers first.
func TestProbeIsolatesFailuresAndPreservesOrder(t *testing.T) {
	srv := fakeDevice(t, 4)
	defer srv.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	dead.Close() // closed immediately, so connections are refused

	r := New(&config.Config{Devices: []config.Device{
		{Name: "dead", Address: strings.TrimPrefix(dead.URL, "http://"), Source: config.SourceCable},
		{Name: "antenna", Address: strings.TrimPrefix(srv.URL, "http://"), Source: config.SourceAntenna},
	}})

	states := r.Probe(context.Background())
	if len(states) != 2 {
		t.Fatalf("got %d states, want 2", len(states))
	}
	if states[0].Device.Name != "dead" || states[1].Device.Name != "antenna" {
		t.Fatalf("results out of configuration order: %q, %q",
			states[0].Device.Name, states[1].Device.Name)
	}
	if states[0].Err == nil {
		t.Error("unreachable device should report an error")
	}
	if states[1].Err != nil {
		t.Errorf("healthy device failed alongside the dead one: %v", states[1].Err)
	}
}
