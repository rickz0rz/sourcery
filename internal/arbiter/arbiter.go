// Package arbiter decides which physical tuner serves a stream request, and
// refuses when there is none to spare.
package arbiter

import (
	"sync"

	"sourcery/internal/device"
	"sourcery/internal/lineup"
)

// Arbiter tracks tuner capacity across the fleet.
//
// Capacity has two components. Sourcery knows exactly how many streams it is
// holding, because it opened them. It cannot know what else is using the
// devices, so that is learned by polling each device's status.json -- consumers
// bypassing Sourcery entirely are a normal condition, not a fault, and their
// tuners must still count against capacity or the arbiter will over-commit.
type Arbiter struct {
	mu      sync.Mutex
	devices map[string]*deviceCapacity
}

type deviceCapacity struct {
	name    string
	tuners  int // physical tuner count, from discover.json
	held    int // streams Sourcery is currently holding
	foreign int // tuners in use by something else, from the last poll
}

func (d *deviceCapacity) free() int {
	free := d.tuners - d.held - d.foreign
	if free < 0 {
		return 0
	}
	return free
}

// New builds an Arbiter from a fleet probe. Devices that failed to probe are
// omitted, so nothing will be routed to them.
func New(states []device.State) *Arbiter {
	devices := make(map[string]*deviceCapacity, len(states))
	for _, s := range states {
		if s.Err != nil || s.Discover == nil {
			continue
		}
		devices[s.Device.Name] = &deviceCapacity{
			name:   s.Device.Name,
			tuners: s.Discover.TunerCount,
		}
	}
	return &Arbiter{devices: devices}
}

// Lease is a claim on one tuner. Release must be called exactly once, when the
// stream it covers has finished.
type Lease struct {
	Candidate lineup.Candidate

	once sync.Once
	dev  *deviceCapacity
	a    *Arbiter
}

// Release returns the tuner to the pool.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.a.mu.Lock()
		defer l.a.mu.Unlock()
		if l.dev.held > 0 {
			l.dev.held--
		}
	})
}

// TryAcquire claims a tuner on the candidate's device, reporting whether one
// was available.
//
// The claim is provisional: the device is the final authority on whether a
// tuner is really free, since the poll that informs foreign usage is always
// slightly stale. A caller that fails to open the upstream stream must Release
// and try the next candidate.
func (a *Arbiter) TryAcquire(c lineup.Candidate) (*Lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	dev, ok := a.devices[c.Device]
	if !ok || dev.free() < 1 {
		return nil, false
	}
	dev.held++
	return &Lease{Candidate: c, dev: dev, a: a}, true
}

// Reconcile updates a device's foreign usage from a fresh status.json reading.
//
// inUse is the device's own count of busy tuners, which includes the streams
// Sourcery is holding. Subtracting our own leaves what everything else is
// using. The result is clamped at zero because the two figures are sampled at
// slightly different moments and can briefly disagree.
func (a *Arbiter) Reconcile(name string, inUse int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	dev, ok := a.devices[name]
	if !ok {
		return
	}
	if foreign := inUse - dev.held; foreign > 0 {
		dev.foreign = foreign
	} else {
		dev.foreign = 0
	}
}

// Status is a point-in-time view of one device's capacity.
type Status struct {
	Device  string
	Tuners  int
	Held    int // in use by Sourcery
	Foreign int // in use by consumers bypassing Sourcery
	Free    int
}

// Snapshot reports current capacity across the fleet.
func (a *Arbiter) Snapshot() []Status {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]Status, 0, len(a.devices))
	for _, d := range a.devices {
		out = append(out, Status{
			Device:  d.name,
			Tuners:  d.tuners,
			Held:    d.held,
			Foreign: d.foreign,
			Free:    d.free(),
		})
	}
	return out
}
