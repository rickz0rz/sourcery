// Package config loads Sourcery's on-disk configuration.
//
// The format is JSON so that the binary keeps a stdlib-only dependency set.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Source describes where a device gets its signal. It drives source
// preference: when a channel is available from more than one device, the
// scarcer cable tuners are conserved by preferring antenna.
type Source string

const (
	SourceAntenna Source = "antenna"
	SourceCable   Source = "cable"
)

// Rank orders sources by preference, lowest first.
func (s Source) Rank() int {
	if s == SourceAntenna {
		return 0
	}
	return 1
}

// Device is one physical HDHomeRun.
type Device struct {
	// Name is a short operator-facing label, e.g. "prime".
	Name string `json:"name"`
	// Address is the device host, optionally with a port.
	Address string `json:"address"`
	// Source is where this device's signal comes from.
	Source Source `json:"source"`
}

// Config is the whole of Sourcery's configuration.
type Config struct {
	Devices []Device `json:"devices"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Devices) == 0 {
		return fmt.Errorf("no devices configured")
	}

	seenName := make(map[string]bool, len(c.Devices))
	seenAddr := make(map[string]bool, len(c.Devices))

	for i, d := range c.Devices {
		switch {
		case d.Name == "":
			return fmt.Errorf("devices[%d]: name is required", i)
		case d.Address == "":
			return fmt.Errorf("devices[%d] (%s): address is required", i, d.Name)
		case d.Source != SourceAntenna && d.Source != SourceCable:
			return fmt.Errorf("devices[%d] (%s): source must be %q or %q, got %q",
				i, d.Name, SourceAntenna, SourceCable, d.Source)
		case seenName[d.Name]:
			return fmt.Errorf("devices[%d]: duplicate name %q", i, d.Name)
		case seenAddr[d.Address]:
			return fmt.Errorf("devices[%d] (%s): duplicate address %q", i, d.Name, d.Address)
		}
		seenName[d.Name] = true
		seenAddr[d.Address] = true
	}
	return nil
}
