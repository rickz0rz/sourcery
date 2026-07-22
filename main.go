// Command sourcery arbitrates HDHomeRun tuners across consumers that do not
// coordinate with each other.
//
// M1 serves the emulated tuner: consumers can discover Sourcery and scan its
// merged lineup. Streaming arrives in M2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"sourcery/internal/arbiter"
	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
	"sourcery/internal/lineup"
	"sourcery/internal/server"
	"sourcery/internal/stream"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sourcery: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config.json", "path to config file")
	probeOnly := flag.Bool("probe", false, "probe the devices, print a report, and exit")
	verbose := flag.Bool("v", false, "log every request")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	registry := device.New(cfg)

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelProbe()
	states := registry.Probe(probeCtx)

	opts := lineup.Options{
		AllowATSC3: cfg.AllowATSC3,
		Mappings:   cfg.Mappings,
		Exclude:    cfg.Exclude,
		Streams:    cfg.Streams,
	}

	if *probeOnly {
		report(states, opts)
		return reachabilityError(states)
	}
	if err := reachabilityError(states); err != nil {
		return err
	}

	if cfg.UsesFFmpeg() && !stream.NewFFmpeg(cfg.FFmpegPath).Available() {
		log.Warn("a configured stream needs ffmpeg to remux HLS, but the binary was not found; those streams will fail",
			"ffmpeg_path", cfg.FFmpegPath)
	}

	arb := arbiter.New(states)
	srv, err := server.New(cfg, arb, log)
	if err != nil {
		return err
	}

	merged := lineup.Merge(states, opts)
	srv.SetLineup(merged)

	for _, s := range states {
		if s.Err != nil {
			log.Warn("device unreachable; its channels are absent from the lineup",
				"device", s.Device.Name, "address", s.Device.Address, "error", s.Err)
		}
	}
	for _, m := range merged.UnmatchedMappings {
		log.Warn("configured mapping had no effect; check the channel numbers",
			"channel", m.Channel, "source_device", m.Source.Device, "source_channel", m.Source.Channel)
	}
	for _, st := range merged.UnmatchedStreams {
		log.Warn("configured web stream had no effect; its channel is not in the lineup",
			"channel", st.Channel, "url", st.URL)
	}
	log.Info("lineup merged",
		"channels", len(merged.Channels),
		"multi_source", countMultiSource(merged),
		"atsc3_excluded", merged.Excluded.ATSC3,
		"drm_excluded", merged.Excluded.DRM,
		"mobile_excluded", merged.Excluded.Mobile,
		"manually_excluded", merged.Excluded.Manual,
		"grace_period", cfg.Grace())

	return serve(cfg, srv, registry, arb, log)
}

// pollInterval is how often each device is asked what its tuners are doing.
//
// Devices free a tuner within about two seconds of a consumer disconnecting, so
// this is frequent enough to notice capacity returning without generating
// meaningful traffic. It only governs foreign usage: Sourcery's own streams are
// accounted for the moment they start and stop.
const pollInterval = 5 * time.Second

// pollTuners keeps the arbiter's view of foreign tuner usage current.
//
// Consumers that talk to the devices directly are a normal condition, and their
// tuners have to count against capacity or the arbiter will hand out tuners
// that do not exist.
func pollTuners(ctx context.Context, registry *device.Registry, arb *arbiter.Arbiter, log *slog.Logger) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, d := range registry.Devices() {
				reqCtx, cancel := context.WithTimeout(ctx, hdhr.DefaultTimeout)
				tuners, err := d.Client.Status(reqCtx)
				cancel()

				if err != nil {
					log.Warn("tuner poll failed; leaving the last known capacity in place",
						"device", d.Name, "error", err)
					continue
				}
				var inUse int
				for _, t := range tuners {
					if t.Active() {
						inUse++
					}
				}
				arb.Reconcile(d.Name, inUse)
			}
		}
	}
}

func serve(cfg *config.Config, srv *server.Server, registry *device.Registry,
	arb *arbiter.Arbiter, log *slog.Logger,
) error {
	// No write timeout: a stream stays open for as long as someone is
	// watching, and a deadline would cut it off mid-programme.
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go pollTuners(ctx, registry, arb, log)

	errc := make(chan error, 1)
	go func() {
		log.Info("serving emulated tuner",
			"listen", cfg.Listen, "device_id", srv.DeviceID(),
			"friendly_name", cfg.FriendlyName, "tuners_advertised", cfg.TunerCount)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// reachabilityError fails only when nothing at all answered. Losing one device
// degrades the lineup but leaves Sourcery useful.
func reachabilityError(states []device.State) error {
	for _, s := range states {
		if s.Err == nil {
			return nil
		}
	}
	return fmt.Errorf("no devices reachable")
}

func countMultiSource(l lineup.Lineup) int {
	var n int
	for _, c := range l.Channels {
		if len(c.Candidates) > 1 {
			n++
		}
	}
	return n
}

func report(states []device.State, opts lineup.Options) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tADDRESS\tSOURCE\tMODEL\tTUNERS\tCHANNELS\tDRM")

	var totalTuners, totalFree int
	for _, s := range states {
		d := s.Device
		if s.Err != nil {
			fmt.Fprintf(w, "%s\t%s\t%s\tunreachable\t-\t-\t-\n", d.Name, d.Address, d.Source)
			continue
		}
		totalTuners += s.Discover.TunerCount
		totalFree += s.Free()

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d free / %d\t%d\t%d\n",
			d.Name, d.Address, d.Source, s.Discover.ModelNumber,
			s.Free(), s.Discover.TunerCount, len(s.Lineup), s.Protected())
	}
	w.Flush()

	fmt.Printf("\n%d of %d tuners free\n", totalFree, totalTuners)

	// Surface tuners held right now. Anything here that Sourcery did not open
	// is a consumer bypassing it, which its accounting has to respect.
	for _, s := range states {
		for _, t := range s.Tuners {
			if !t.Active() {
				continue
			}
			fmt.Printf("  %s/%s in use by %s (reports channel %s %s)\n",
				s.Device.Name, t.Resource, t.TargetIP, t.VctNumber, t.VctName)
		}
	}

	merged := lineup.Merge(states, opts)
	fmt.Printf("\nmerged lineup: %d channels, %d available from more than one source\n",
		len(merged.Channels), countMultiSource(merged))
	fmt.Printf("excluded: %d ATSC 3.0, %d copy-protected, %d mobile feeds, %d by rule\n",
		merged.Excluded.ATSC3, merged.Excluded.DRM, merged.Excluded.Mobile, merged.Excluded.Manual)
	for _, m := range merged.UnmatchedMappings {
		fmt.Printf("WARNING: mapping for channel %s from %s:%s matched nothing\n",
			m.Channel, m.Source.Device, m.Source.Channel)
	}
	for _, st := range merged.UnmatchedStreams {
		fmt.Printf("WARNING: web stream for channel %s is not in the lineup\n", st.Channel)
	}

	for _, c := range merged.Channels {
		if len(c.Candidates) < 2 {
			continue
		}
		fmt.Printf("  %-8s %-12s", c.Number, c.Name)
		for i, cand := range c.Candidates {
			if i > 0 {
				fmt.Print(" -> ")
			}
			fmt.Printf(" %s:%s", cand.Device, cand.GuideNumber)
		}
		fmt.Println()
	}

	for _, s := range states {
		if s.Err != nil {
			fmt.Printf("\n%s: %v\n", s.Device.Name, s.Err)
		}
	}
}
