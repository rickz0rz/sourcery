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
	// Listen is the address Sourcery's emulated tuner serves on. Port 5004 is
	// what HDHomeRun devices use, so consumers expect to find it there.
	Listen string `json:"listen,omitempty"`

	// FriendlyName is how Sourcery introduces itself to consumers.
	FriendlyName string `json:"friendly_name,omitempty"`

	// TunerCount is the tuner count Sourcery advertises. Because stream reuse
	// lets more concurrent consumers succeed than there are physical tuners,
	// this need not equal the fleet total.
	TunerCount int `json:"tuner_count,omitempty"`

	// DeviceID overrides the derived device ID, as eight hex digits. It must
	// satisfy the HDHomeRun checksum. Leave unset to derive one.
	DeviceID string `json:"device_id,omitempty"`

	// AdvertiseAddress overrides the host:port Sourcery puts in the stream URLs
	// it hands out. Leave unset to use whatever host the consumer asked for,
	// which is correct on a normal LAN.
	AdvertiseAddress string `json:"advertise_address,omitempty"`

	Devices []Device `json:"devices"`
}

// Defaults applied when the config leaves a field unset.
const (
	DefaultListen       = ":5004"
	DefaultFriendlyName = "Sourcery"
	DefaultTunerCount   = 7
)

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
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.FriendlyName == "" {
		c.FriendlyName = DefaultFriendlyName
	}
	if c.TunerCount == 0 {
		c.TunerCount = DefaultTunerCount
	}
}

func (c *Config) validate() error {
	if len(c.Devices) == 0 {
		return fmt.Errorf("no devices configured")
	}
	if c.TunerCount < 1 {
		return fmt.Errorf("tuner_count must be at least 1, got %d", c.TunerCount)
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
