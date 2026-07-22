package relay

import (
	"sync"
	"time"

	"sourcery/internal/arbiter"
	"sourcery/internal/lineup"
	"sourcery/internal/stream"
)

// broadcast is one upstream connection fanned out to its subscribers.
//
// Exactly one reader goroutine (run) pulls from the upstream and pushes to each
// subscriber. Subscribers attach via join and detach via remove. When the last
// one leaves, the upstream is held open for a grace period so a consumer that
// flips away and back, or a DVR probing during a scan, can reattach without
// re-tuning; only when the grace expires is the upstream closed and the tuner
// freed.
type broadcast struct {
	hub   *Hub
	key   string // upstream URL, the hub's map key
	cand  lineup.Candidate
	lease *arbiter.Lease
	grace time.Duration

	// ready is closed once the open attempt finishes. Until then subscribers
	// wait; err is set (before ready is closed) if the open failed.
	ready chan struct{}
	up    Upstream
	err   error

	mu         sync.Mutex
	subs       map[*subscriber]struct{}
	graceTimer *time.Timer // running while idle within the grace period
	draining   bool        // upstream is being torn down; no new subscribers
	closed     bool        // run has finished; the broadcast is dead
}

// join adds a subscriber, or returns nil if the broadcast is no longer
// accepting them because it is being torn down. Attaching during the grace
// period cancels the pending teardown -- this is the fast reattach.
func (b *broadcast) join(consumer string) *subscriber {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining || b.closed {
		return nil
	}
	b.stopGraceLocked()
	sub := &subscriber{ch: make(chan []byte, subscriberBuffer), consumer: consumer}
	b.subs[sub] = struct{}{}
	return sub
}

// remove detaches a subscriber. If it was the last one, the grace period begins.
func (b *broadcast) remove(sub *subscriber) {
	b.mu.Lock()
	if _, ok := b.subs[sub]; ok {
		delete(b.subs, sub)
		close(sub.ch)
	}
	empty := len(b.subs) == 0 && !b.draining && !b.closed
	b.mu.Unlock()

	if empty {
		b.idle()
	}
}

// idle is called when the last subscriber leaves. With no grace configured the
// upstream is torn down at once; otherwise a timer holds it open for a while in
// case a consumer returns.
func (b *broadcast) idle() {
	if b.grace <= 0 {
		b.beginDrain()
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining || b.closed || len(b.subs) > 0 || b.graceTimer != nil {
		return
	}
	b.graceTimer = time.AfterFunc(b.grace, b.graceExpired)
}

// graceExpired tears the upstream down if it is still idle when the grace
// period ends. A consumer that reattached in the meantime cancels this.
func (b *broadcast) graceExpired() {
	b.mu.Lock()
	b.graceTimer = nil
	drain := len(b.subs) == 0 && !b.draining && !b.closed
	if drain {
		b.draining = true
	}
	b.mu.Unlock()

	if drain {
		b.up.Close() // unblocks run's Read
	}
}

// beginDrain closes the upstream immediately if it is idle.
func (b *broadcast) beginDrain() {
	b.mu.Lock()
	drain := len(b.subs) == 0 && !b.draining && !b.closed
	if drain {
		b.draining = true
	}
	b.mu.Unlock()

	if drain {
		b.up.Close()
	}
}

// stopGraceLocked cancels a pending grace teardown. The caller holds b.mu.
func (b *broadcast) stopGraceLocked() {
	if b.graceTimer != nil {
		b.graceTimer.Stop()
		b.graceTimer = nil
	}
}

// run reads the upstream and fans each chunk out until the stream ends or the
// last subscriber leaves.
func (b *broadcast) run() {
	defer b.finish()

	for {
		// A fresh buffer per read: the slice is handed to every subscriber and
		// read concurrently, so it must not be overwritten by the next read.
		buf := make([]byte, stream.ReadSize)
		n, err := b.up.Read(buf)
		if n > 0 {
			b.fanout(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// fanout delivers a chunk to every subscriber without blocking on any of them.
// A subscriber whose buffer is full has fallen too far behind and is dropped,
// so one slow consumer can never stall the upstream or the others.
func (b *broadcast) fanout(chunk []byte) {
	b.mu.Lock()
	for sub := range b.subs {
		select {
		case sub.ch <- chunk:
		default:
			delete(b.subs, sub)
			close(sub.ch)
			b.hub.log.Warn("dropping consumer that fell behind the stream",
				"device", b.cand.Device, "device_channel", b.cand.GuideNumber)
		}
	}
	empty := len(b.subs) == 0 && !b.draining && !b.closed
	b.mu.Unlock()

	// Dropping the last slow consumer leaves the stream idle; enter grace like
	// any other emptying, so a brief blip does not cost an immediate re-tune.
	if empty {
		b.idle()
	}
}

// finish tears the broadcast down: it leaves the hub's registry, releases the
// tuner, and closes out any subscribers still attached (as when the device
// ended the stream rather than the consumers leaving).
func (b *broadcast) finish() {
	b.hub.mu.Lock()
	if b.hub.streams[b.key] == b {
		delete(b.hub.streams, b.key)
	}
	b.hub.mu.Unlock()

	b.up.Close()
	b.lease.Release()

	b.mu.Lock()
	b.closed = true
	b.stopGraceLocked()
	for sub := range b.subs {
		delete(b.subs, sub)
		close(sub.ch)
	}
	b.mu.Unlock()
}
