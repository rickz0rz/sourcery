// Package device holds the set of physical HDHomeRuns Sourcery manages and
// probes their current state.
package device

import (
	"context"
	"sync"

	"sourcery/internal/config"
	"sourcery/internal/hdhr"
)

// Device pairs a configured device with a client for talking to it.
type Device struct {
	config.Device
	Client *hdhr.Client
}

// State is a point-in-time snapshot of one device.
//
// Err is set when probing failed; the remaining fields are then unpopulated. A
// device that is unreachable is a normal operating condition, not a fatal one:
// the rest of the fleet should keep working.
type State struct {
	Device     *Device
	Discover   *hdhr.Discover
	Lineup     []hdhr.Channel
	Tuners     []hdhr.Tuner
	ScanStatus *hdhr.LineupStatus
	Err        error
}

// InUse counts tuners currently held on this device, including any held by
// consumers bypassing Sourcery.
func (s State) InUse() int {
	var n int
	for _, t := range s.Tuners {
		if t.Active() {
			n++
		}
	}
	return n
}

// Free reports how many tuners remain available.
func (s State) Free() int {
	if s.Discover == nil {
		return 0
	}
	return s.Discover.TunerCount - s.InUse()
}

// Protected counts copy-protected channels in the lineup.
func (s State) Protected() int {
	var n int
	for _, c := range s.Lineup {
		if c.Protected() {
			n++
		}
	}
	return n
}

// Registry is the fixed set of devices Sourcery manages.
type Registry struct {
	devices []*Device
}

// New builds a Registry from configuration.
func New(cfg *config.Config) *Registry {
	devices := make([]*Device, 0, len(cfg.Devices))
	for _, d := range cfg.Devices {
		devices = append(devices, &Device{Device: d, Client: hdhr.NewClient(d.Address)})
	}
	return &Registry{devices: devices}
}

// Devices returns the managed devices in configuration order.
func (r *Registry) Devices() []*Device { return r.devices }

// Probe snapshots every device concurrently. Results are returned in
// configuration order regardless of completion order, and a failure against one
// device never fails the others.
func (r *Registry) Probe(ctx context.Context) []State {
	states := make([]State, len(r.devices))

	var wg sync.WaitGroup
	for i, d := range r.devices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[i] = probe(ctx, d)
		}()
	}
	wg.Wait()

	return states
}

func probe(ctx context.Context, d *Device) State {
	s := State{Device: d}

	disc, err := d.Client.Discover(ctx)
	if err != nil {
		s.Err = err
		return s
	}
	s.Discover = disc

	if s.Lineup, err = d.Client.Lineup(ctx); err != nil {
		s.Err = err
		return s
	}
	if s.Tuners, err = d.Client.Status(ctx); err != nil {
		s.Err = err
		return s
	}
	if s.ScanStatus, err = d.Client.LineupStatus(ctx); err != nil {
		s.Err = err
		return s
	}
	return s
}
