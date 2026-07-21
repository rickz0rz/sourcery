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

	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/lineup"
	"sourcery/internal/server"
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

	opts := lineup.Options{AllowATSC3: cfg.AllowATSC3}

	if *probeOnly {
		report(states, opts)
		return reachabilityError(states)
	}
	if err := reachabilityError(states); err != nil {
		return err
	}

	srv, err := server.New(cfg, log)
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
	log.Info("lineup merged",
		"channels", len(merged.Channels),
		"multi_source", countMultiSource(merged),
		"atsc3_excluded", merged.Excluded.ATSC3,
		"drm_excluded", merged.Excluded.DRM,
		"mobile_excluded", merged.Excluded.Mobile)

	return serve(cfg, srv, log)
}

func serve(cfg *config.Config, srv *server.Server, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	fmt.Printf("excluded: %d ATSC 3.0, %d copy-protected, %d mobile feeds\n",
		merged.Excluded.ATSC3, merged.Excluded.DRM, merged.Excluded.Mobile)

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
