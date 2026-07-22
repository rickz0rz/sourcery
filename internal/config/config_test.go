package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, `{"devices":[
		{"name":"antenna","address":"192.0.2.10","source":"antenna"},
		{"name":"cable","address":"192.0.2.11","source":"cable"}
	]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(cfg.Devices))
	}
	if cfg.Devices[0].Source != SourceAntenna {
		t.Errorf("source = %q, want antenna", cfg.Devices[0].Source)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{{
		name: "no devices",
		body: `{"devices":[]}`,
		want: "no devices configured",
	}, {
		name: "missing name",
		body: `{"devices":[{"address":"192.0.2.1","source":"cable"}]}`,
		want: "name is required",
	}, {
		name: "missing address",
		body: `{"devices":[{"name":"a","source":"cable"}]}`,
		want: "address is required",
	}, {
		name: "unknown source",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"satellite"}]}`,
		want: "source must be",
	}, {
		name: "duplicate name",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable"},{"name":"a","address":"192.0.2.2","source":"antenna"}]}`,
		want: "duplicate name",
	}, {
		// Two entries for one device would double-count its tuners.
		name: "duplicate address",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable"},{"name":"b","address":"192.0.2.1","source":"antenna"}]}`,
		want: "duplicate address",
	}, {
		// Catches typo'd keys that would otherwise be silently ignored.
		name: "unknown field",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable","tuners":3}]}`,
		want: "unknown field",
	}, {
		name: "malformed json",
		body: `{"devices":`,
		want: "parse",
	}, {
		name: "bad grace period",
		body: `{"grace_period":"soon","devices":[{"name":"a","address":"192.0.2.1","source":"cable"}]}`,
		want: "duration",
	}, {
		name: "negative grace period",
		body: `{"grace_period":"-5s","devices":[{"name":"a","address":"192.0.2.1","source":"cable"}]}`,
		want: "negative",
	}, {
		name: "mapping to unknown device",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable"}],"mappings":[{"channel":"2","source":{"device":"ghost","channel":"2.1"}}]}`,
		want: "not a configured device",
	}, {
		name: "mapping missing channel",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable"}],"mappings":[{"source":{"device":"a","channel":"2.1"}}]}`,
		want: "channel is required",
	}, {
		name: "exclude unknown device",
		body: `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable"}],"exclude":[{"device":"ghost","channel":"9"}]}`,
		want: "not a configured device",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.body))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestAntennaOutranksCable(t *testing.T) {
	if SourceAntenna.Rank() >= SourceCable.Rank() {
		t.Error("antenna should outrank cable to conserve scarcer cable tuners")
	}
}

func TestGracePeriodDefaultsAndOverrides(t *testing.T) {
	base := `{"devices":[{"name":"a","address":"192.0.2.1","source":"cable"}]`

	unset, err := Load(write(t, base+"}"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if unset.Grace() != DefaultGracePeriod {
		t.Errorf("unset grace = %v, want the default %v", unset.Grace(), DefaultGracePeriod)
	}

	// An explicit "0s" is honoured, meaning immediate teardown.
	zero, err := Load(write(t, base+`,"grace_period":"0s"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if zero.Grace() != 0 {
		t.Errorf("explicit 0s grace = %v, want 0", zero.Grace())
	}

	set, err := Load(write(t, base+`,"grace_period":"45s"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Grace() != 45*time.Second {
		t.Errorf("grace = %v, want 45s", set.Grace())
	}
}

func TestMappingsAndExcludeLoad(t *testing.T) {
	cfg, err := Load(write(t, `{
		"devices":[
			{"name":"antenna","address":"192.0.2.10","source":"antenna"},
			{"name":"cable","address":"192.0.2.11","source":"cable"}
		],
		"mappings":[{"channel":"294","source":{"device":"antenna","channel":"4.2"}}],
		"exclude":[{"device":"cable","channel":"999"}]
	}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Mappings) != 1 || cfg.Mappings[0].Channel != "294" {
		t.Errorf("mappings = %+v", cfg.Mappings)
	}
	if len(cfg.Exclude) != 1 || cfg.Exclude[0].Channel != "999" {
		t.Errorf("exclude = %+v", cfg.Exclude)
	}
}
