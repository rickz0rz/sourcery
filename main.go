// Command sourcery arbitrates HDHomeRun tuners across consumers that do not
// coordinate with each other.
//
// M0 provides configuration, the device registry, and a one-shot probe. It does
// not serve anything yet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"sourcery/internal/config"
	"sourcery/internal/device"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sourcery: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	states := device.New(cfg).Probe(ctx)
	report(states)

	// A probe that reached nothing means Sourcery has no tuners to arbitrate,
	// which is worth a non-zero exit. Partial reachability is not.
	var reached int
	for _, s := range states {
		if s.Err == nil {
			reached++
		}
	}
	if reached == 0 {
		return fmt.Errorf("no devices reachable")
	}
	return nil
}

func report(states []device.State) {
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

	for _, s := range states {
		if s.Err != nil {
			fmt.Printf("\n%s: %v\n", s.Device.Name, s.Err)
		}
	}
}
