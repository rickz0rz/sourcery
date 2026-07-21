package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
