package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sourcery/internal/arbiter"
	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
	"sourcery/internal/lineup"
)

// fakeUpstream is an in-memory stand-in for a device stream. It emits chunks on
// demand and unblocks its readers when closed, mirroring how a real device's
// connection behaves when the tuner is released.
type fakeUpstream struct {
	feed   chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{feed: make(chan []byte, 64), closed: make(chan struct{})}
}

func (f *fakeUpstream) push(b []byte) { f.feed <- b }

func (f *fakeUpstream) Read(p []byte) (int, error) {
	select {
	case b := <-f.feed:
		return copy(p, b), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeUpstream) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// fakeProxy hands out fakeUpstreams and records how many opens happened per URL,
// which is how the tests detect a tuner being taken more than once.
type fakeProxy struct {
	mu       sync.Mutex
	streams  map[string]*fakeUpstream
	opens    map[string]int
	headers  map[string]map[string]string // headers seen per URL
	failURLs map[string]bool
	openCh   chan struct{} // if non-nil, Open blocks until it is signalled
}

func newFakeProxy() *fakeProxy {
	return &fakeProxy{
		streams:  make(map[string]*fakeUpstream),
		opens:    make(map[string]int),
		headers:  make(map[string]map[string]string),
		failURLs: make(map[string]bool),
	}
}

func (p *fakeProxy) Open(ctx context.Context, url string, headers map[string]string) (Upstream, error) {
	if p.openCh != nil {
		<-p.openCh
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opens[url]++
	p.headers[url] = headers
	if p.failURLs[url] {
		return nil, errors.New("device refused")
	}
	f := newFakeUpstream()
	p.streams[url] = f
	return f, nil
}

func (p *fakeProxy) openCount(url string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opens[url]
}

func (p *fakeProxy) upstream(url string) *fakeUpstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streams[url]
}

func testHub(t *testing.T, proxy *fakeProxy, tuners map[string]int) *Hub {
	t.Helper()
	return testHubGrace(t, proxy, tuners, 0)
}

func testHubGrace(t *testing.T, proxy *fakeProxy, tuners map[string]int, grace time.Duration) *Hub {
	t.Helper()
	var states []device.State
	for name, count := range tuners {
		states = append(states, device.State{
			Device:   &device.Device{Device: config.Device{Name: name, Source: config.SourceAntenna}},
			Discover: &hdhr.Discover{TunerCount: count},
		})
	}
	return NewHub(arbiter.New(states), proxy, slog.New(slog.DiscardHandler), grace)
}

func channel(name string, cands ...lineup.Candidate) lineup.Channel {
	return lineup.Channel{Number: name, Name: name, Candidates: cands}
}

func cand(device, number, url string) lineup.Candidate {
	return lineup.Candidate{Device: device, GuideNumber: number, URL: url}
}

func webCand(url string, headers map[string]string) lineup.Candidate {
	return lineup.Candidate{Device: "web", URL: url, Web: true, Headers: headers}
}

func TestSingleConsumerOpensOneStream(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 2})

	sub, err := hub.Subscribe(channel("2.1", cand("antenna", "2.1", "u")), "test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if sub.Reused {
		t.Error("the first consumer should not be a reuse")
	}
	if proxy.openCount("u") != 1 {
		t.Errorf("opened %d streams, want 1", proxy.openCount("u"))
	}
}

// The reason M3 exists: several consumers of one channel share one tuner.
func TestReuseSharesOneStream(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 4})

	ch := channel("2.1", cand("antenna", "2.1", "u"))
	subs := make([]*Subscription, 3)
	for i := range subs {
		var err error
		subs[i], err = hub.Subscribe(ch, "test")
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		defer subs[i].Close()
	}

	if proxy.openCount("u") != 1 {
		t.Errorf("opened %d streams for one channel, want 1", proxy.openCount("u"))
	}
	if subs[0].Reused {
		t.Error("first consumer wrongly marked as reuse")
	}
	if !subs[1].Reused || !subs[2].Reused {
		t.Error("later consumers should be reuses")
	}

	// A pushed chunk reaches every consumer.
	proxy.upstream("u").push([]byte("hello"))
	for i, s := range subs {
		select {
		case got := <-s.Chunks():
			if string(got) != "hello" {
				t.Errorf("consumer %d got %q, want hello", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("consumer %d received nothing", i)
		}
	}
}

// Two presented channels that resolve to the same device feed share a tuner:
// the reuse key is the upstream, not the channel number.
func TestReuseAcrossChannelsSharingASource(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 4})

	// Channel 4 and 232 both prefer the same antenna feed.
	sub1, err := hub.Subscribe(channel("4", cand("antenna", "4.1", "shared"), cand("cable", "4", "c4")), "test")
	if err != nil {
		t.Fatalf("Subscribe 4: %v", err)
	}
	defer sub1.Close()

	sub2, err := hub.Subscribe(channel("232", cand("antenna", "4.1", "shared"), cand("cable", "232", "c232")), "test")
	if err != nil {
		t.Fatalf("Subscribe 232: %v", err)
	}
	defer sub2.Close()

	if !sub2.Reused {
		t.Error("the second channel should have reused the shared antenna feed")
	}
	if proxy.openCount("shared") != 1 {
		t.Errorf("opened the shared feed %d times, want 1", proxy.openCount("shared"))
	}
}

// The race M3 is most at risk of: many simultaneous requests for the same
// channel must converge on one tuner, not open one apiece.
func TestConcurrentRequestsShareOneTuner(t *testing.T) {
	proxy := newFakeProxy()
	proxy.openCh = make(chan struct{}) // hold opens so all requests pile up first
	hub := testHub(t, proxy, map[string]int{"antenna": 8})

	ch := channel("2.1", cand("antenna", "2.1", "u"))

	const n = 20
	var wg sync.WaitGroup
	var reused atomic.Int32
	subs := make([]*Subscription, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := hub.Subscribe(ch, "test")
			subs[i], errs[i] = s, err
			if err == nil && s.Reused {
				reused.Add(1)
			}
		}()
	}

	// Let every goroutine reach the open before releasing it.
	time.Sleep(50 * time.Millisecond)
	close(proxy.openCh)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		defer subs[i].Close()
	}
	if got := proxy.openCount("u"); got != 1 {
		t.Errorf("opened %d streams under a stampede, want 1", got)
	}
	if reused.Load() != n-1 {
		t.Errorf("%d reuses, want %d (all but the creator)", reused.Load(), n-1)
	}
}

// When a stream is not shareable, capacity still bounds it.
func TestDistinctChannelsExhaustTuners(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 1})

	a, err := hub.Subscribe(channel("2.1", cand("antenna", "2.1", "a")), "test")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	defer a.Close()

	// A different upstream on the one-tuner device cannot be served.
	if _, err := hub.Subscribe(channel("5.1", cand("antenna", "5.1", "b")), "test"); !errors.Is(err, ErrNoTuner) {
		t.Errorf("err = %v, want ErrNoTuner", err)
	}
}

// A candidate whose open fails is skipped for the next, since the arbiter's
// view of foreign usage is always slightly stale.
func TestFallsThroughToWorkingCandidate(t *testing.T) {
	proxy := newFakeProxy()
	proxy.failURLs["broken"] = true
	hub := testHub(t, proxy, map[string]int{"antenna": 4, "cable": 4})

	sub, err := hub.Subscribe(channel("2",
		cand("antenna", "2.1", "broken"),
		cand("cable", "2", "works"),
	), "test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if sub.Candidate.Device != "cable" {
		t.Errorf("served from %q, want the working cable source", sub.Candidate.Device)
	}
	// The failed attempt must not have leaked its tuner.
	for _, st := range hub.arbiter.Snapshot() {
		if st.Held > 1 {
			t.Errorf("%s holds %d tuners, want at most 1", st.Device, st.Held)
		}
	}
}

// A consumer too slow to keep up is dropped, and must not stall the others.
func TestSlowConsumerIsDroppedNotBlocking(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 2})

	ch := channel("2.1", cand("antenna", "2.1", "u"))
	slow, _ := hub.Subscribe(ch, "test")
	fast, _ := hub.Subscribe(ch, "test")
	defer fast.Close()

	up := proxy.upstream("u")

	// The slow consumer never reads. Push more than its buffer holds; the fast
	// consumer, draining continuously, must keep receiving throughout.
	got := make(chan int, 1)
	go func() {
		var n int
		for range fast.Chunks() {
			n++
			if n == subscriberBuffer+10 {
				got <- n
				return
			}
		}
		got <- n
	}()

	for i := 0; i < subscriberBuffer+10; i++ {
		up.push([]byte{byte(i)})
	}

	select {
	case n := <-got:
		if n != subscriberBuffer+10 {
			t.Errorf("fast consumer received %d chunks, want %d", n, subscriberBuffer+10)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fast consumer was stalled by the slow one")
	}

	// The slow consumer's channel should have been closed when it was dropped.
	waitClosed(t, slow.Chunks())
}

func waitClosed(t *testing.T, ch <-chan []byte) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("slow consumer was not dropped")
		}
	}
}

// The tuner comes back when the last consumer leaves.
func TestTunerReleasedWhenLastConsumerLeaves(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 1})

	ch := channel("2.1", cand("antenna", "2.1", "u"))
	a, _ := hub.Subscribe(ch, "test")
	b, _ := hub.Subscribe(ch, "test")

	a.Close()
	if h := held(hub, "antenna"); h != 1 {
		t.Errorf("held = %d after one of two left, want 1", h)
	}
	b.Close()

	waitFor(t, func() bool { return held(hub, "antenna") == 0 },
		"tuner to be released after the last consumer left")

	// And the stream is gone from the registry, so the next request opens fresh.
	c, err := hub.Subscribe(ch, "test")
	if err != nil {
		t.Fatalf("Subscribe after teardown: %v", err)
	}
	defer c.Close()
	if proxy.openCount("u") != 2 {
		t.Errorf("opened %d times total, want 2 (a fresh open after teardown)", proxy.openCount("u"))
	}
}

// Within the grace period a returning consumer reattaches to the still-open
// upstream, so no second tuner is taken -- the point of the grace window.
func TestGracePeriodAllowsReattachWithoutRetuning(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHubGrace(t, proxy, map[string]int{"antenna": 1}, time.Minute)

	ch := channel("2.1", cand("antenna", "2.1", "u"))
	first, _ := hub.Subscribe(ch, "test")
	first.Close() // last consumer leaves; grace begins

	// The tuner is still held during grace, and a return reuses the stream.
	if h := held(hub, "antenna"); h != 1 {
		t.Errorf("held = %d during grace, want the tuner kept", h)
	}
	second, err := hub.Subscribe(ch, "test")
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	defer second.Close()

	if !second.Reused {
		t.Error("the returning consumer should have reused the held stream")
	}
	if proxy.openCount("u") != 1 {
		t.Errorf("opened %d times, want 1 (no re-tune within grace)", proxy.openCount("u"))
	}
}

// When the grace period passes with nobody watching, the tuner is released.
func TestGracePeriodExpiresAndReleasesTuner(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHubGrace(t, proxy, map[string]int{"antenna": 1}, 30*time.Millisecond)

	ch := channel("2.1", cand("antenna", "2.1", "u"))
	sub, _ := hub.Subscribe(ch, "test")
	sub.Close()

	waitFor(t, func() bool { return held(hub, "antenna") == 0 },
		"the tuner to be released after the grace period")
}

// A web stream is used only when the tuner ahead of it is gone, takes no tuner
// lease, and carries its configured headers.
func TestWebStreamIsLastResortAndCarriesHeaders(t *testing.T) {
	proxy := newFakeProxy()
	hub := testHub(t, proxy, map[string]int{"antenna": 1})

	headers := map[string]string{"Referer": "https://example.test/"}
	// The web-backed channel prefers its tuner, then falls to the web stream.
	webChannel := channel("5.1",
		cand("antenna", "5.1", "tunerB"),
		webCand("https://example.test/live.ts", headers),
	)

	// Occupy the single tuner with a different channel, so the web-backed
	// channel cannot get a tuner and cannot reuse an existing stream.
	other, err := hub.Subscribe(channel("2.1", cand("antenna", "2.1", "tunerA")), "a")
	if err != nil {
		t.Fatalf("occupying stream: %v", err)
	}
	defer other.Close()

	// Its tuner candidate cannot be acquired, so it falls through to the web
	// stream -- which needs no tuner.
	second, err := hub.Subscribe(webChannel, "b")
	if err != nil {
		t.Fatalf("web-backed subscribe: %v", err)
	}
	defer second.Close()

	if !second.Candidate.Web {
		t.Fatalf("consumer used %q, want the web fallback", second.Candidate.Device)
	}
	// The web stream took no tuner: the device still shows just the one held.
	if h := held(hub, "antenna"); h != 1 {
		t.Errorf("held = %d, want 1 (the web stream must not take a tuner)", h)
	}
	if got := proxy.headers["https://example.test/live.ts"]["Referer"]; got != "https://example.test/" {
		t.Errorf("Referer sent = %q, want the configured value", got)
	}
}

func held(hub *Hub, device string) int {
	for _, st := range hub.arbiter.Snapshot() {
		if st.Device == device {
			return st.Held
		}
	}
	return -1
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
