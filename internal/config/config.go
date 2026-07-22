// Package config loads Sourcery's on-disk configuration.
//
// The format is JSON so that the binary keeps a stdlib-only dependency set.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Duration is a time.Duration that reads from JSON as a string like "10s".
// The standard library marshals durations as nanosecond integers, which are
// unreadable in a hand-edited config.
type Duration time.Duration

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`duration must be a string like "10s": %w`, err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	*d = Duration(parsed)
	return nil
}

// ChannelRef identifies one channel on one device by its guide number.
type ChannelRef struct {
	Device  string `json:"device"`
	Channel string `json:"channel"`
}

// Mapping manually attaches a source channel to a presented channel, for the
// stations that automatic matching cannot connect -- an antenna feed called
// "H&I" that a cable provider lists as "WDIVDT2", say.
type Mapping struct {
	// Channel is the presented channel number to attach the source to.
	Channel string `json:"channel"`
	// Source is the device and guide number to attach as an alternate route.
	Source ChannelRef `json:"source"`
}

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

	// AllowATSC3 admits next-generation broadcast channels into the lineup.
	// They are excluded by default: their AC4 audio does not play reliably on
	// the consumers, and every one of them shadows an ATSC 1.0 channel that
	// does play.
	AllowATSC3 bool `json:"allow_atsc3,omitempty"`

	// AdvertiseAddress overrides the host:port Sourcery puts in the stream URLs
	// it hands out. Leave unset to use whatever host the consumer asked for,
	// which is correct on a normal LAN.
	AdvertiseAddress string `json:"advertise_address,omitempty"`

	// GracePeriod is how long an upstream is kept open after its last consumer
	// leaves, so a consumer that flips away and back, or a DVR probing during a
	// scan, reattaches without re-tuning. Nil means the default; an explicit
	// "0s" tears streams down immediately.
	GracePeriod *Duration `json:"grace_period,omitempty"`

	// Mappings manually attach sources to presented channels that automatic
	// matching does not connect.
	Mappings []Mapping `json:"mappings,omitempty"`

	// Exclude drops specific channels from the lineup by device and number.
	Exclude []ChannelRef `json:"exclude,omitempty"`

	Devices []Device `json:"devices"`
}

// Defaults applied when the config leaves a field unset.
const (
	DefaultListen       = ":5004"
	DefaultFriendlyName = "Sourcery"
	DefaultTunerCount   = 7
	DefaultGracePeriod  = 10 * time.Second
)

// Grace returns the configured grace period, or the default when unset.
func (c *Config) Grace() time.Duration {
	if c.GracePeriod == nil {
		return DefaultGracePeriod
	}
	return time.Duration(*c.GracePeriod)
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

	for i, m := range c.Mappings {
		if m.Channel == "" {
			return fmt.Errorf("mappings[%d]: channel is required", i)
		}
		if m.Source.Channel == "" {
			return fmt.Errorf("mappings[%d]: source.channel is required", i)
		}
		if !seenName[m.Source.Device] {
			return fmt.Errorf("mappings[%d]: source.device %q is not a configured device", i, m.Source.Device)
		}
	}
	for i, e := range c.Exclude {
		if e.Channel == "" {
			return fmt.Errorf("exclude[%d]: channel is required", i)
		}
		if !seenName[e.Device] {
			return fmt.Errorf("exclude[%d]: device %q is not a configured device", i, e.Device)
		}
	}
	return nil
}
