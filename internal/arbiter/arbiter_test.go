package arbiter

import (
	"errors"
	"sync"
	"testing"

	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
	"sourcery/internal/lineup"
)

func fleet(t *testing.T, tuners map[string]int) *Arbiter {
	t.Helper()
	var states []device.State
	for name, count := range tuners {
		states = append(states, device.State{
			Device:   &device.Device{Device: config.Device{Name: name, Source: config.SourceAntenna}},
			Discover: &hdhr.Discover{TunerCount: count},
		})
	}
	return New(states)
}

func cand(dev string) lineup.Candidate {
	return lineup.Candidate{Device: dev, GuideNumber: "2.1"}
}

func status(t *testing.T, a *Arbiter, name string) Status {
	t.Helper()
	for _, s := range a.Snapshot() {
		if s.Device == name {
			return s
		}
	}
	t.Fatalf("no device %q in snapshot", name)
	return Status{}
}

func TestAcquireUntilExhausted(t *testing.T) {
	a := fleet(t, map[string]int{"antenna": 2})

	first, ok := a.TryAcquire(cand("antenna"))
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := a.TryAcquire(cand("antenna")); !ok {
		t.Fatal("second acquire failed")
	}
	if _, ok := a.TryAcquire(cand("antenna")); ok {
		t.Error("third acquire succeeded on a two-tuner device")
	}

	first.Release()
	if _, ok := a.TryAcquire(cand("antenna")); !ok {
		t.Error("acquire failed after a release freed a tuner")
	}
}

func TestUnknownDeviceIsNeverAcquired(t *testing.T) {
	a := fleet(t, map[string]int{"antenna": 2})
	if _, ok := a.TryAcquire(cand("nonexistent")); ok {
		t.Error("acquired a tuner on a device that is not in the fleet")
	}
}

// A device that failed to probe has no known capacity, so nothing may be routed
// to it.
func TestUnreachableDeviceGetsNoCapacity(t *testing.T) {
	a := New([]device.State{{
		Device: &device.Device{Device: config.Device{Name: "cable"}},
		Err:    errors.New("unreachable"),
	}})
	if _, ok := a.TryAcquire(cand("cable")); ok {
		t.Error("acquired a tuner on an unreachable device")
	}
}

// Releasing twice must not hand back a tuner that was never held, or capacity
// would inflate over time.
func TestReleaseIsIdempotent(t *testing.T) {
	a := fleet(t, map[string]int{"antenna": 1})

	lease, _ := a.TryAcquire(cand("antenna"))
	lease.Release()
	lease.Release()

	if held := status(t, a, "antenna").Held; held != 0 {
		t.Errorf("Held = %d, want 0", held)
	}
	if _, ok := a.TryAcquire(cand("antenna")); !ok {
		t.Fatal("acquire failed after release")
	}
	if _, ok := a.TryAcquire(cand("antenna")); ok {
		t.Error("a double release inflated capacity")
	}
}

// status.json counts Sourcery's own streams too, so foreign usage is what
// remains after subtracting them.
func TestReconcileSeparatesForeignFromOwnUsage(t *testing.T) {
	a := fleet(t, map[string]int{"cable": 3})

	lease, _ := a.TryAcquire(cand("cable"))

	// The device reports two busy tuners: ours plus somebody else's.
	a.Reconcile("cable", 2)

	st := status(t, a, "cable")
	if st.Held != 1 || st.Foreign != 1 || st.Free != 1 {
		t.Errorf("got held=%d foreign=%d free=%d, want 1/1/1", st.Held, st.Foreign, st.Free)
	}

	// Our stream ends. The foreign tuner is still out there.
	lease.Release()
	if st := status(t, a, "cable"); st.Free != 2 {
		t.Errorf("Free = %d, want 2 after releasing our own stream", st.Free)
	}
}

// Own-count and device-count are sampled at slightly different moments, so the
// subtraction can go negative. It must not become spare capacity.
func TestReconcileClampsAtZero(t *testing.T) {
	a := fleet(t, map[string]int{"cable": 3})

	l1, _ := a.TryAcquire(cand("cable"))
	l2, _ := a.TryAcquire(cand("cable"))
	defer l1.Release()
	defer l2.Release()

	a.Reconcile("cable", 1) // stale reading, taken before our second stream

	st := status(t, a, "cable")
	if st.Foreign != 0 {
		t.Errorf("Foreign = %d, want 0 rather than a negative count", st.Foreign)
	}
	if st.Free != 1 {
		t.Errorf("Free = %d, want 1", st.Free)
	}
}

// A device fully occupied by other consumers offers nothing, even though
// Sourcery holds none of it.
func TestForeignUsageExhaustsCapacity(t *testing.T) {
	a := fleet(t, map[string]int{"cable": 3})
	a.Reconcile("cable", 3)

	if _, ok := a.TryAcquire(cand("cable")); ok {
		t.Error("acquired a tuner while every one was in use elsewhere")
	}
	if free := status(t, a, "cable").Free; free != 0 {
		t.Errorf("Free = %d, want 0", free)
	}
}

func TestReconcileIgnoresUnknownDevice(t *testing.T) {
	a := fleet(t, map[string]int{"antenna": 2})
	a.Reconcile("nonexistent", 5) // must not panic or create capacity
	if len(a.Snapshot()) != 1 {
		t.Error("reconciling an unknown device altered the fleet")
	}
}

// Concurrent consumers must never be handed more tuners than exist.
func TestConcurrentAcquireRespectsCapacity(t *testing.T) {
	const tuners = 4
	a := fleet(t, map[string]int{"antenna": tuners})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var granted []*Lease

	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lease, ok := a.TryAcquire(cand("antenna")); ok {
				mu.Lock()
				granted = append(granted, lease)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(granted) != tuners {
		t.Errorf("granted %d leases, want exactly %d", len(granted), tuners)
	}
	for _, l := range granted {
		l.Release()
	}
	if held := status(t, a, "antenna").Held; held != 0 {
		t.Errorf("Held = %d after releasing everything", held)
	}
}
