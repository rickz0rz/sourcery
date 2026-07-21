// Package lineup merges the per-device channel lineups into the single unified
// lineup Sourcery presents to consumers.
package lineup

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

	"sourcery/internal/config"
	"sourcery/internal/device"
	"sourcery/internal/hdhr"
)

// Candidate is one concrete way to receive a logical channel: a specific
// channel number on a specific device.
type Candidate struct {
	Device      string        // configured device name
	Source      config.Source // where that device gets its signal
	GuideNumber string        // the channel number on that device
	URL         string        // upstream stream URL on that device
	VideoCodec  string
	AudioCodec  string
	HD          bool
}

// Channel is a logical channel with every known way to receive it, best first.
type Channel struct {
	Number     string // the number Sourcery presents to consumers
	Name       string
	HD         bool
	Candidates []Candidate
}

// Lineup is the merged view across all devices.
type Lineup struct {
	Channels []Channel

	// Excluded counts what was dropped and why.
	Excluded Exclusions
}

// Exclusions breaks down the channels left out of the merged lineup.
type Exclusions struct {
	DRM    int // copy-protected, so unplayable by the consumers
	Mobile int // ATSC 3.0 streams tailored to handheld devices
}

// Total returns the number of excluded channels.
func (e Exclusions) Total() int { return e.DRM + e.Mobile }

// mobileVariant reports whether a channel is an ATSC 3.0 stream intended for
// handheld devices rather than for television.
//
// Broadcasters put these on a .99 subchannel and mark the callsign with a MOB
// suffix -- here, 107.99 WXYZMOB and 120.99 WMYDMOB. They are low-resolution
// companion feeds, so they are never what a DVR wants.
func mobileVariant(ch hdhr.Channel) bool {
	if _, minor := splitChannelNumber(ch.GuideNumber); minor == 99 {
		return true
	}
	return strings.HasSuffix(normalizeName(ch.GuideName), "MOB")
}

// compatible reports whether a codec pair is broadly playable by the consumers.
//
// The ATSC 3.0 channels on the antenna device are HEVC video with AC4 audio,
// which Plex and Channels handle poorly or not at all, while their ATSC 1.0
// twins are MPEG2/AC3. Preferring the compatible pair matters more than any
// tuner-economy preference: a stream that will not play is worth nothing.
func compatible(videoCodec string) bool {
	return strings.EqualFold(videoCodec, "MPEG2")
}

// rankCandidate orders the ways of receiving one logical channel. Lower sorts
// first. The ordering is, in priority order:
//
//  1. playable codec before exotic codec
//  2. antenna before cable, to conserve the scarcer cable tuners
//  3. lower channel number, which is usually the canonical listing
func rankCandidate(a, b Candidate) int {
	if n := cmp.Compare(codecRank(a), codecRank(b)); n != 0 {
		return n
	}
	if n := cmp.Compare(a.Source.Rank(), b.Source.Rank()); n != 0 {
		return n
	}
	return compareChannelNumbers(a.GuideNumber, b.GuideNumber)
}

func codecRank(c Candidate) int {
	if compatible(c.VideoCodec) {
		return 0
	}
	return 1
}

// broadcastSuffixes are transmission-type designators the antenna device
// appends to callsigns: WDIV-HD, WWJ-HD, CHWI_HD, WDWO-CD, WUDT-LD. They
// describe how a station is transmitted, not what it is showing, so they are
// dropped when matching.
var broadcastSuffixes = []string{"HD", "SD", "DT", "LD", "CD", "TV"}

// normalizeName reduces a guide name to a merge key.
//
// Matching is deliberately conservative: case folding, dropping a trailing
// broadcast-type suffix, then stripping punctuation. It merges "Movies!" with
// "MOVIES" and the antenna's "WDIV-HD" with cable's "WDIV", but not "WDIV" with
// "WDIVDT". Under-merging is the safe failure -- it costs a source preference
// opportunity, whereas over-merging routes a consumer to the wrong programme.
// Anything it misses is what the manual overrides are for.
//
// The suffix is only dropped when an explicit separator sets it off. Cable
// names its own HD variants without one -- QVC and QVCHD are genuinely
// different channels there -- and those must not collapse into each other.
func normalizeName(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))

	if i := strings.LastIndexAny(upper, "-_ "); i > 0 {
		if slices.Contains(broadcastSuffixes, upper[i+1:]) {
			upper = upper[:i]
		}
	}

	var b strings.Builder
	for _, r := range upper {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Merge builds the unified lineup from a fleet probe.
//
// Entries are merged only across devices, never within one. Two different
// channel numbers on the same device are two different channels, even when
// they share a name: the antenna carries seven distinct WDWO-CD subchannels at
// 18.1 through 18.7, and collapsing those by name would silently delete six
// channels. Cross-device matches are the ones worth merging, because they are
// what makes source preference and failover possible.
//
// Two classes of channel are dropped outright rather than merely ranked low,
// because there is no circumstance in which routing a consumer to them helps:
//
//   - Copy-protected channels, which the consumers cannot play at all. In
//     practice this is mostly ATSC 3.0, and it removes the DRM-bearing
//     duplicates of channels that remain available over ATSC 1.0.
//   - ATSC 3.0 mobile companion feeds, which are meant for handheld devices.
func Merge(states []device.State) Lineup {
	groups := make(map[string][]Candidate)
	names := make(map[string]string) // merge key -> first-seen display name

	var excluded Exclusions

	for _, s := range states {
		if s.Err != nil {
			continue
		}
		for _, ch := range s.Lineup {
			switch {
			case ch.Protected():
				excluded.DRM++
				continue
			case mobileVariant(ch):
				excluded.Mobile++
				continue
			}
			key := normalizeName(ch.GuideName)
			if key == "" {
				continue
			}
			groups[key] = append(groups[key], Candidate{
				Device:      s.Device.Name,
				Source:      s.Device.Source,
				GuideNumber: ch.GuideNumber,
				URL:         ch.URL,
				VideoCodec:  ch.VideoCodec,
				AudioCodec:  ch.AudioCodec,
				HD:          ch.HD != 0,
			})
			if _, ok := names[key]; !ok {
				names[key] = ch.GuideName
			}
		}
	}

	var channels []Channel
	for key, cands := range groups {
		primary, extra := splitByDevice(cands)

		slices.SortStableFunc(primary, rankCandidate)
		channels = append(channels, Channel{
			Number:     primary[0].GuideNumber, // the best candidate names the channel
			Name:       names[key],
			HD:         anyHD(primary),
			Candidates: primary,
		})

		// Same name, same device, different number: a distinct channel that
		// happens to share a callsign. It stands on its own.
		for _, c := range extra {
			channels = append(channels, Channel{
				Number:     c.GuideNumber,
				Name:       names[key],
				HD:         c.HD,
				Candidates: []Candidate{c},
			})
		}
	}

	slices.SortStableFunc(channels, func(a, b Channel) int {
		if n := compareChannelNumbers(a.Number, b.Number); n != 0 {
			return n
		}
		return cmp.Compare(a.Name, b.Name)
	})

	return Lineup{Channels: channels, Excluded: excluded}
}

// splitByDevice picks the best candidate from each device as the merged
// channel's alternatives, and returns every other entry separately so it can
// stand as its own channel.
func splitByDevice(cands []Candidate) (primary, extra []Candidate) {
	byDevice := make(map[string][]Candidate)
	for _, c := range cands {
		byDevice[c.Device] = append(byDevice[c.Device], c)
	}

	for _, name := range slices.Sorted(maps.Keys(byDevice)) { // deterministic
		group := byDevice[name]
		slices.SortStableFunc(group, rankCandidate)
		primary = append(primary, group[0])
		extra = append(extra, group[1:]...)
	}
	return primary, extra
}

func anyHD(cands []Candidate) bool {
	for _, c := range cands {
		if c.HD {
			return true
		}
	}
	return false
}

// compareChannelNumbers orders numbers like "2", "2.1" and "102.1" the way a
// viewer expects, rather than lexically, where "10" would precede "9".
func compareChannelNumbers(a, b string) int {
	aMaj, aMin := splitChannelNumber(a)
	bMaj, bMin := splitChannelNumber(b)
	if n := cmp.Compare(aMaj, bMaj); n != 0 {
		return n
	}
	if n := cmp.Compare(aMin, bMin); n != 0 {
		return n
	}
	return cmp.Compare(a, b)
}

func splitChannelNumber(s string) (major, minor int) {
	maj, min, _ := strings.Cut(s, ".")
	major, _ = strconv.Atoi(maj)
	minor, _ = strconv.Atoi(min)
	return major, minor
}

// Find returns the channel presented at the given number.
func (l Lineup) Find(number string) (Channel, bool) {
	for _, c := range l.Channels {
		if c.Number == number {
			return c, true
		}
	}
	return Channel{}, false
}

// AsHDHomeRun renders the merged lineup in the wire format consumers expect,
// with every stream URL pointing back at Sourcery rather than at a device.
func (l Lineup) AsHDHomeRun(baseURL string) []hdhr.Channel {
	out := make([]hdhr.Channel, 0, len(l.Channels))
	for _, c := range l.Channels {
		var isHD int
		if c.HD {
			isHD = 1
		}
		best := c.Candidates[0]
		out = append(out, hdhr.Channel{
			GuideNumber: c.Number,
			GuideName:   c.Name,
			VideoCodec:  best.VideoCodec,
			AudioCodec:  best.AudioCodec,
			HD:          isHD,
			URL:         baseURL + "/auto/v" + c.Number,
		})
	}
	return out
}
