// Package stream relays MPEG transport streams from a device to a consumer.
//
// Sourcery has to stay in the data path rather than redirecting: an HDHomeRun
// allocates one tuner per HTTP connection, so a redirected consumer would open
// its own connection and consume a second tuner for a channel already being
// received. Relaying is also what makes reuse possible later, since one
// upstream connection can feed several consumers.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ContentType is what HDHomeRun devices serve and what consumers expect.
const ContentType = "video/mp2t"

// bufferSize is a whole number of 188-byte transport stream packets, close to
// 64 KiB. Copying in packet-aligned chunks keeps partial packets out of the
// buffer boundaries.
const bufferSize = 188 * 348

// Proxy opens upstream streams and relays them.
type Proxy struct {
	client *http.Client
}

// NewProxy returns a Proxy.
//
// The client deliberately has no overall timeout: a stream is open for as long
// as someone is watching, and a deadline would cut it off mid-programme. Only
// the connect and response-header phases are bounded, which is where a wedged
// device actually shows up.
func NewProxy() *Proxy {
	return &Proxy{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				ResponseHeaderTimeout: 10 * time.Second,
				DisableCompression:    true,
			},
		},
	}
}

// Upstream is an open stream from a device.
type Upstream struct {
	body io.ReadCloser
}

// Open starts a stream. The device allocates a tuner at this point and holds it
// until the response body is closed.
//
// A device with no free tuner answers with an error status rather than
// blocking, which is how the caller learns that its capacity accounting was
// stale and it should try another candidate.
func (p *Proxy) Open(ctx context.Context, url string) (*Upstream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build stream request for %s: %w", url, err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open stream %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("open stream %s: device answered %s", url, resp.Status)
	}
	return &Upstream{body: resp.Body}, nil
}

// CopyTo relays the stream to w until the consumer disconnects, the device
// stops sending, or the request context is cancelled. It returns the number of
// bytes relayed.
//
// Each chunk is flushed rather than buffered, because a consumer waiting on a
// live stream should not have to wait for a buffer to fill before playback
// starts.
func (u *Upstream) CopyTo(w http.ResponseWriter) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, bufferSize)

	var total int64
	for {
		n, readErr := u.body.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				// The consumer went away. Normal, and not worth reporting as a
				// failure: it is how every stream ends.
				return total, nil
			}
			if err := rc.Flush(); err != nil {
				return total, nil
			}
		}
		if readErr != nil {
			// A consumer disconnecting cancels the request, which cancels the
			// upstream read. That is how essentially every stream ends, so it
			// is not a failure -- reporting it as one would bury the real
			// errors among the routine ones.
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, context.Canceled) {
				return total, nil
			}
			return total, readErr
		}
	}
}

// Close releases the upstream connection, and with it the device's tuner.
func (u *Upstream) Close() error { return u.body.Close() }
