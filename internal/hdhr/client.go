package hdhr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultTimeout bounds every control-plane request. Devices on a healthy LAN
// answer in single-digit milliseconds; anything slower is a fault worth
// surfacing rather than waiting on.
const DefaultTimeout = 5 * time.Second

// Client talks to a single HDHomeRun device's JSON control endpoints. It is
// safe for concurrent use.
//
// This covers the control plane only. Streaming is deliberately not handled
// here: the device allocates one tuner per HTTP stream connection, so stream
// lifetime is an arbitration concern, not a client concern.
type Client struct {
	addr string
	http *http.Client
}

// NewClient returns a Client for the device at addr, which may be a bare host
// or host:port.
func NewClient(addr string) *Client {
	return &Client{
		addr: addr,
		http: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

// Addr returns the device address this client was built for.
func (c *Client) Addr() string { return c.addr }

// Discover fetches device identity and tuner count.
func (c *Client) Discover(ctx context.Context) (*Discover, error) {
	var d Discover
	if err := c.get(ctx, "/discover.json", &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Lineup fetches the device's full channel lineup.
func (c *Client) Lineup(ctx context.Context) ([]Channel, error) {
	var ch []Channel
	if err := c.get(ctx, "/lineup.json", &ch); err != nil {
		return nil, err
	}
	return ch, nil
}

// Status fetches per-tuner state, including tuners held by consumers that are
// bypassing us entirely.
func (c *Client) Status(ctx context.Context) ([]Tuner, error) {
	var t []Tuner
	if err := c.get(ctx, "/status.json", &t); err != nil {
		return nil, err
	}
	return t, nil
}

// LineupStatus fetches scan state and the device's configured signal source.
func (c *Client) LineupStatus(ctx context.Context) (*LineupStatus, error) {
	var s LineupStatus
	if err := c.get(ctx, "/lineup_status.json", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	url := "http://" + c.addr + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", url, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: unexpected status %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}
