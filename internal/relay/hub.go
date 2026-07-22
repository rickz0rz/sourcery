// Package relay shares one upstream stream among several consumers.
//
// An HDHomeRun allocates a tuner per connection, so without this a channel
// watched by three consumers would occupy three tuners. The hub keeps one
// connection open per upstream and fans its bytes out to every consumer of it,
// so the same three share a single tuner.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"sourcery/internal/arbiter"
	"sourcery/internal/lineup"
)

// ErrNoTuner means every candidate source for a channel was busy. It is
// distinct from an upstream failure so the caller can answer appropriately.
var ErrNoTuner = errors.New("no tuner available")

// errCollision is internal: another request is already starting a broadcast for
// the same upstream, so this request should loop back and join it.
var errCollision = errors.New("broadcast already starting")

// subscriberBuffer bounds how far behind a single consumer may fall before it
// is dropped. At the read size this is a few megabytes and a few seconds of
// video -- enough to ride out jitter, but a consumer that stays this far behind
// a real-time broadcast is genuinely too slow to keep up.
const subscriberBuffer = 32

// maxAttempts bounds the join/create retry loop. Retries happen only when a
// broadcast is torn down at the same moment another request tries to join it,
// which resolves within an attempt or two; the cap is a backstop against a
// pathological churn loop, not a normal path.
const maxAttempts = 8

// Upstream is a live stream from a device. Read yields the transport stream;
// Close releases the connection and, with it, the tuner, and unblocks any Read
// in progress. *stream.Upstream is the production implementation.
type Upstream interface {
	Read(p []byte) (int, error)
	Close() error
}

// Opener starts an upstream stream. Keeping this an interface lets the hub be
// tested without real devices, and keeps the reuse logic independent of the
// transport.
type Opener interface {
	Open(ctx context.Context, url string) (Upstream, error)
}

// Hub tracks the live upstreams and their subscribers.
type Hub struct {
	arbiter *arbiter.Arbiter
	proxy   Opener
	log     *slog.Logger

	mu      sync.Mutex
	streams map[string]*broadcast // keyed by upstream URL
}

// NewHub builds a Hub.
func NewHub(arb *arbiter.Arbiter, proxy Opener, log *slog.Logger) *Hub {
	return &Hub{
		arbiter: arb,
		proxy:   proxy,
		log:     log,
		streams: make(map[string]*broadcast),
	}
}

// Subscription is one consumer's view of a stream.
type Subscription struct {
	// Candidate is the source actually being used, for logging.
	Candidate lineup.Candidate
	// Reused reports whether this joined an existing stream rather than opening
	// a new one -- i.e. whether it cost a tuner.
	Reused bool

	sub *subscriber
	b   *broadcast
}

// Chunks yields the stream's data. It is closed when the stream ends, whether
// because the device stopped, the upstream failed, or this consumer was
// dropped for falling too far behind.
func (s *Subscription) Chunks() <-chan []byte { return s.sub.ch }

// Close detaches this consumer. When the last consumer of an upstream leaves,
// the upstream is closed and its tuner released.
func (s *Subscription) Close() { s.b.remove(s.sub) }

type subscriber struct {
	ch chan []byte
}

// Subscribe attaches a consumer to the given channel, reusing a live upstream
// when one already serves any of the channel's candidate sources, and opening
// a new one otherwise.
//
// Reuse is preferred over source preference: joining an existing stream costs
// no tuner at all, which conserves capacity better than picking the nominally
// preferred source would.
func (h *Hub) Subscribe(ch lineup.Channel) (*Subscription, error) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if b := h.existing(ch); b != nil {
			<-b.ready
			if b.err != nil {
				continue // that start failed and removed itself; try again
			}
			if sub := b.join(); sub != nil {
				return &Subscription{Candidate: b.cand, Reused: true, sub: sub, b: b}, nil
			}
			continue // it was torn down between ready and join; try again
		}

		sub, err := h.create(ch)
		if errors.Is(err, errCollision) {
			continue // someone else is starting the same upstream; join it
		}
		return sub, err
	}
	return nil, ErrNoTuner
}

// existing returns a live broadcast for one of the channel's candidates, if any.
func (h *Hub) existing(ch lineup.Channel) *broadcast {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cand := range ch.Candidates {
		if b, ok := h.streams[cand.URL]; ok {
			return b
		}
	}
	return nil
}

// create opens a new upstream for the first candidate that has a free tuner and
// answers. Candidates whose open fails are skipped, since the arbiter's view of
// foreign usage is always slightly stale and the device is the final authority.
func (h *Hub) create(ch lineup.Channel) (*Subscription, error) {
	tried := make(map[string]bool)

	for {
		h.mu.Lock()
		// If any not-yet-tried candidate is mid-creation elsewhere, join it
		// rather than opening a competing connection to the same source.
		for _, cand := range ch.Candidates {
			if !tried[cand.URL] {
				if _, busy := h.streams[cand.URL]; busy {
					h.mu.Unlock()
					return nil, errCollision
				}
			}
		}

		var b *broadcast
		for _, cand := range ch.Candidates {
			if tried[cand.URL] {
				continue
			}
			lease, ok := h.arbiter.TryAcquire(cand)
			if !ok {
				continue
			}
			b = &broadcast{
				hub:   h,
				key:   cand.URL,
				cand:  cand,
				lease: lease,
				ready: make(chan struct{}),
				subs:  make(map[*subscriber]struct{}),
			}
			h.streams[cand.URL] = b
			break
		}
		h.mu.Unlock()

		if b == nil {
			return nil, ErrNoTuner
		}

		// Open with a background context: the upstream outlives the request that
		// happened to start it, so it must not be cancelled when that consumer
		// disconnects. Connect and header timeouts still bound it.
		up, err := h.proxy.Open(context.Background(), b.cand.URL)
		if err != nil {
			b.lease.Release()
			h.mu.Lock()
			if h.streams[b.key] == b {
				delete(h.streams, b.key)
			}
			h.mu.Unlock()
			b.err = err
			close(b.ready)

			h.log.Warn("upstream refused; trying the next source",
				"device", b.cand.Device, "device_channel", b.cand.GuideNumber, "error", err)
			tried[b.key] = true
			continue
		}

		b.up = up
		sub := &subscriber{ch: make(chan []byte, subscriberBuffer)}
		b.subs[sub] = struct{}{}
		close(b.ready)
		go b.run()

		return &Subscription{Candidate: b.cand, Reused: false, sub: sub, b: b}, nil
	}
}

// Snapshot reports the live streams and how many consumers each has.
type Snapshot struct {
	Candidate   lineup.Candidate
	Subscribers int
}

// Snapshot returns a view of the current broadcasts.
func (h *Hub) Snapshot() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]Snapshot, 0, len(h.streams))
	for _, b := range h.streams {
		b.mu.Lock()
		n := len(b.subs)
		b.mu.Unlock()
		out = append(out, Snapshot{Candidate: b.cand, Subscribers: n})
	}
	return out
}
