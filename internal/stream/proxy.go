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
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ContentType is what HDHomeRun devices serve and what consumers expect.
const ContentType = "video/mp2t"

// ReadSize is a whole number of 188-byte transport stream packets, close to
// 64 KiB. Reading in packet-aligned chunks keeps partial packets off the
// boundaries, which matters when the same chunk is fanned out to several
// consumers.
const ReadSize = 188 * 348

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

// Open starts a stream. For a device, it allocates a tuner at this point and
// holds it until the response body is closed. headers are applied to the
// request, which is how a web stream that requires a particular Referer or
// User-Agent is satisfied; it is nil for device streams.
//
// A device with no free tuner answers with an error status rather than
// blocking, which is how the caller learns that its capacity accounting was
// stale and it should try another candidate.
func (p *Proxy) Open(ctx context.Context, url string, headers map[string]string) (*Upstream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build stream request for %s: %w", url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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

// Read reads the next chunk of the stream. It returns io.EOF when the device
// stops sending, and a context or network error if the connection is closed
// underneath it -- which is exactly how the reader is told to stop when the
// last consumer of a stream disconnects.
func (u *Upstream) Read(p []byte) (int, error) { return u.body.Read(p) }

// Close releases the upstream connection, and with it the device's tuner.
// Closing also unblocks a Read in progress, which is how a fan-out reader is
// stopped once nobody is watching.
func (u *Upstream) Close() error { return u.body.Close() }
