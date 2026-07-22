// Package server presents Sourcery to consumers as a single HDHomeRun tuner.
//
// The endpoints and their field names are dictated by what Plex and Channels
// DVR expect to find when they talk to an HDHomeRun; they are not ours to
// choose.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"sourcery/internal/arbiter"
	"sourcery/internal/config"
	"sourcery/internal/hdhr"
	"sourcery/internal/lineup"
	"sourcery/internal/relay"
	"sourcery/internal/stream"
)

// Server serves the emulated tuner.
type Server struct {
	cfg      *config.Config
	deviceID string
	log      *slog.Logger
	arbiter  *arbiter.Arbiter
	hub      *relay.Hub

	mu      sync.RWMutex
	current lineup.Lineup
}

// New builds a Server. The device ID is taken from configuration when set and
// derived from the managed devices otherwise, so that it stays stable across
// restarts either way.
func New(cfg *config.Config, arb *arbiter.Arbiter, log *slog.Logger) (*Server, error) {
	var id uint32
	if cfg.DeviceID != "" {
		parsed, err := hdhr.ParseDeviceID(cfg.DeviceID)
		if err != nil {
			return nil, err
		}
		id = parsed
	} else {
		seeds := make([]string, 0, len(cfg.Devices))
		for _, d := range cfg.Devices {
			seeds = append(seeds, d.Address)
		}
		id = hdhr.DeriveDeviceID(seeds)
	}

	return &Server{
		cfg:      cfg,
		deviceID: hdhr.FormatDeviceID(id),
		log:      log,
		arbiter:  arb,
		hub:      relay.NewHub(arb, proxyOpener{stream.NewProxy()}, log, cfg.Grace()),
	}, nil
}

// proxyOpener adapts a *stream.Proxy to relay.Opener, whose Open returns the
// relay.Upstream interface rather than the concrete type.
type proxyOpener struct{ p *stream.Proxy }

func (o proxyOpener) Open(ctx context.Context, url string) (relay.Upstream, error) {
	up, err := o.p.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	return up, nil
}

// DeviceID returns the identity Sourcery presents to consumers.
func (s *Server) DeviceID() string { return s.deviceID }

// SetLineup swaps in a freshly merged lineup.
func (s *Server) SetLineup(l lineup.Lineup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = l
}

// Lineup returns the current merged lineup.
func (s *Server) Lineup() lineup.Lineup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Handler returns the HTTP routes for the emulated tuner.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /discover.json", s.handleDiscover)
	mux.HandleFunc("GET /lineup.json", s.handleLineup)
	mux.HandleFunc("GET /lineup_status.json", s.handleLineupStatus)
	mux.HandleFunc("GET /device.xml", s.handleDeviceXML)
	mux.HandleFunc("POST /lineup.post", s.handleLineupPost)
	mux.HandleFunc("GET /auto/{channel}", s.handleStream)
	mux.HandleFunc("GET /", s.handleRoot)
	return s.withLogging(mux)
}

// baseURL is the address to hand back in stream URLs. Deriving it from the
// request Host means Sourcery works without being told its own address, which
// matters when it moves or is reached by name rather than by IP.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.AdvertiseAddress != "" {
		return "http://" + s.cfg.AdvertiseAddress
	}
	return "http://" + r.Host
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, hdhr.Discover{
		FriendlyName:    s.cfg.FriendlyName,
		ModelNumber:     "HDTC-2US",
		FirmwareName:    "bin_ATSC",
		FirmwareVersion: "20260101",
		DeviceID:        s.deviceID,
		BaseURL:         base,
		LineupURL:       base + "/lineup.json",
		TunerCount:      s.cfg.TunerCount,
	})
}

func (s *Server) handleLineup(w http.ResponseWriter, r *http.Request) {
	channels := s.Lineup().AsHDHomeRun(s.baseURL(r))
	if channels == nil {
		channels = []hdhr.Channel{} // an empty lineup must marshal as [], not null
	}
	writeJSON(w, channels)
}

// handleLineupStatus reports a lineup that is never scanning. Sourcery derives
// its lineup from the devices rather than scanning for it, so a consumer that
// waits for a scan to finish should see it already complete.
func (s *Server) handleLineupStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, hdhr.LineupStatus{
		ScanInProgress: 0,
		ScanPossible:   0,
		Source:         "Cable",
		SourceList:     []string{"Cable"},
	})
}

// handleLineupPost accepts scan requests and does nothing, successfully. Some
// consumers trigger a scan as part of setup and treat a failure as fatal.
func (s *Server) handleLineupPost(w http.ResponseWriter, r *http.Request) {
	s.log.Info("lineup scan requested; ignoring, lineup is derived from devices",
		"consumer", clientIP(r), "scan", r.URL.Query().Get("scan"))
	w.WriteHeader(http.StatusOK)
}

// deviceXML is the UPnP description some consumers fetch during discovery. The
// manufacturer and model fields identify the protocol being spoken, not the
// origin of this software; consumers check them before proceeding.
const deviceXML = `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <URLBase>%s</URLBase>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>Silicondust</manufacturer>
    <modelName>HDTC-2US</modelName>
    <modelNumber>HDTC-2US</modelNumber>
    <serialNumber></serialNumber>
    <UDN>uuid:%s</UDN>
  </device>
</root>
`

func (s *Server) handleDeviceXML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprintf(w, deviceXML, s.baseURL(r), s.cfg.FriendlyName, s.deviceID)
}

// handleStream routes a stream request to the best available source, reusing an
// upstream that is already open for the channel when one exists.
//
// Stream paths are of the form /auto/v2.1, where the "v" prefix means "tune by
// virtual channel number" and is not part of the number itself.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	number := strings.TrimPrefix(r.PathValue("channel"), "v")
	consumer := clientIP(r)

	ch, ok := s.Lineup().Find(number)
	if !ok {
		s.log.Warn("stream requested for a channel that is not in the lineup",
			"consumer", consumer, "channel", number)
		http.Error(w, "no such channel", http.StatusNotFound)
		return
	}

	sub, err := s.hub.Subscribe(ch, consumer)
	if err != nil {
		// Never tear down a stream in flight to make room for a new one.
		// Refusing is honest, and the log line is what makes contention visible.
		s.log.Warn("no tuner available for stream request",
			"consumer", consumer, "channel", number, "name", ch.Name,
			"candidates", len(ch.Candidates), "capacity", s.arbiter.Snapshot(), "error", err)
		http.Error(w, "no tuner available", http.StatusServiceUnavailable)
		return
	}
	defer sub.Close()

	s.log.Info("streaming",
		"consumer", consumer, "channel", ch.Number, "name", ch.Name,
		"device", sub.Candidate.Device, "source", sub.Candidate.Source,
		"device_channel", sub.Candidate.GuideNumber, "reused", sub.Reused)

	w.Header().Set("Content-Type", stream.ContentType)
	w.WriteHeader(http.StatusOK)

	started := time.Now()
	n := s.pump(w, r, sub)

	s.log.Info("stream ended",
		"consumer", consumer, "channel", ch.Number,
		"device", sub.Candidate.Device, "bytes", n,
		"seconds", int(time.Since(started).Seconds()))
}

// pump forwards a subscription's chunks to the consumer until the stream ends
// or the consumer disconnects. It returns the number of bytes written.
//
// Each chunk is flushed rather than buffered, because a consumer waiting on a
// live stream should not wait for a buffer to fill before playback starts.
func (s *Server) pump(w http.ResponseWriter, r *http.Request, sub *relay.Subscription) int64 {
	rc := http.NewResponseController(w)
	var total int64
	for {
		select {
		case chunk, ok := <-sub.Chunks():
			if !ok {
				return total // the stream ended, or this consumer was dropped
			}
			n, err := w.Write(chunk)
			total += int64(n)
			if err != nil {
				return total // the consumer went away; how essentially every stream ends
			}
			if err := rc.Flush(); err != nil {
				return total
			}
		case <-r.Context().Done():
			return total
		}
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	l := s.Lineup()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s\ndevice id %s\n%d channels, %d tuners advertised\n\n",
		s.cfg.FriendlyName, s.deviceID, len(l.Channels), s.cfg.TunerCount)

	fmt.Fprintln(w, "tuners:")
	for _, st := range s.arbiter.Snapshot() {
		fmt.Fprintf(w, "  %-8s %d free of %d  (%d held by sourcery, %d elsewhere)\n",
			st.Device, st.Free, st.Tuners, st.Held, st.Foreign)
	}

	if live := s.hub.Snapshot(); len(live) > 0 {
		fmt.Fprintln(w, "\nlive streams:")
		for _, st := range live {
			if st.Idle {
				fmt.Fprintf(w, "  %s ch %s  idle, held for reuse\n",
					st.Candidate.Device, st.Candidate.GuideNumber)
				continue
			}
			fmt.Fprintf(w, "  %s ch %s  %d consumer(s) on 1 tuner: %s\n",
				st.Candidate.Device, st.Candidate.GuideNumber, st.Subscribers,
				strings.Join(st.Consumers, ", "))
		}
	}
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path,
			"consumer", clientIP(r), "agent", r.UserAgent())
		next.ServeHTTP(w, r)
	})
}

// clientIP identifies the consumer behind a request. Source IP is how Sourcery
// tells its three consumers apart, and it matches the TargetIP that devices
// report for streams opened directly against them.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already written by now, so this can only be logged.
		return
	}
}
