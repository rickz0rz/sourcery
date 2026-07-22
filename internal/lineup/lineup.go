// Package lineup merges the per-device channel lineups into the single unified
// lineup Sourcery presents to consumers.
package lineup

import (
	"cmp"
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
	Device      string        // configured device name, or "web" for a web stream
	Source      config.Source // where that device gets its signal
	GuideNumber string        // the channel number on that device
	URL         string        // upstream stream URL on that device
	VideoCodec  string
	AudioCodec  string
	HD          bool

	// Web marks a candidate that is an external stream rather than a device
	// tuner: it consumes no tuner and is only ever used as a last resort.
	Web bool
	// Headers are extra request headers for a web stream, e.g. a required
	// Referer. Empty for device candidates.
	Headers map[string]string
}

// Channel is a logical channel: an identity as consumers see it, plus every
// known way to receive it, best first.
//
// Identity and routing are deliberately separate. Number and Name come from the
// spine device so that the lineup a consumer sees matches the guide data it
// fetches, while Candidates are ordered by what is cheapest and best to stream
// right now. A consumer asking for cable channel 4 may well be served from the
// antenna without ever knowing an antenna is involved.
type Channel struct {
	Number     string // the number Sourcery presents to consumers
	Name       string
	HD         bool
	Candidates []Candidate

	spine bool // identity came from the comprehensive lineup
}

// FromSpine reports whether this channel came from the comprehensive lineup
// rather than existing only on an alternate device.
func (c Channel) FromSpine() bool { return c.spine }

// Lineup is the merged view across all devices.
type Lineup struct {
	Channels []Channel

	// Excluded counts what was dropped and why.
	Excluded Exclusions

	// UnmatchedMappings are configured mappings whose target channel or source
	// could not be found, so the operator can be told they had no effect.
	UnmatchedMappings []config.Mapping

	// UnmatchedStreams are configured web streams whose target channel does not
	// exist in the lineup.
	UnmatchedStreams []config.Stream
}

// Exclusions breaks down the channels left out of the merged lineup.
type Exclusions struct {
	ATSC3  int // next-generation broadcast, off by default
	DRM    int // copy-protected, so unplayable by the consumers
	Mobile int // ATSC 3.0 streams tailored to handheld devices
	Manual int // dropped by a configured exclude rule
}

// Total returns the number of excluded channels.
func (e Exclusions) Total() int { return e.ATSC3 + e.DRM + e.Mobile + e.Manual }

// Options tunes what the merged lineup contains.
type Options struct {
	// AllowATSC3 admits next-generation broadcast channels. It is off by
	// default: their AC4 audio does not play reliably on the consumers, and a
	// channel that cannot be played is worse than one that is simply absent.
	AllowATSC3 bool

	// Mappings manually attach sources to presented channels that automatic
	// matching does not connect.
	Mappings []config.Mapping

	// Exclude drops specific channels by device and guide number.
	Exclude []config.ChannelRef

	// Streams attach external web streams as a last-resort source.
	Streams []config.Stream
}

// refKey identifies a channel on a device for exclusion and mapping lookups.
func refKey(device, number string) string { return device + "\x00" + number }

// isATSC3 reports whether a channel is a next-generation broadcast.
//
// HEVC video is the reliable signature: on this fleet it selects exactly the
// seven ATSC 3.0 entries and nothing else. AC4 is checked too so an AC4 stream
// carried over some other video codec is still caught.
//
// H264 is deliberately not a signal. Cable carries 184 ordinary H264/AC3
// channels, and treating that codec as next-generation would exclude a third
// of the lineup.
func isATSC3(ch hdhr.Channel) bool {
	return strings.EqualFold(ch.VideoCodec, "HEVC") || strings.EqualFold(ch.AudioCodec, "AC4")
}

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

// compatible reports whether a video codec is broadly playable by the
// consumers. MPEG2 and H264 both are, and between them they cover every
// ATSC 1.0 and cable channel on this fleet; HEVC, which means ATSC 3.0, is not.
//
// This only matters when AllowATSC3 is set, since otherwise those channels are
// dropped before ranking. Preferring a playable stream outranks any
// tuner-economy preference: one that will not play is worth nothing.
func compatible(videoCodec string) bool {
	return strings.EqualFold(videoCodec, "MPEG2") || strings.EqualFold(videoCodec, "H264")
}

// rankCandidate orders the ways of receiving one logical channel. Lower sorts
// first. The ordering is, in priority order:
//
//  1. a real tuner before a web stream, which is only ever a last resort
//  2. playable codec before exotic codec
//  3. high definition before standard definition
//  4. antenna before cable, to conserve the scarcer cable tuners
//  5. lower channel number, which is usually the canonical listing
//
// A web stream ranks below every tuner because it is an off-air fallback of
// unknown quality and reliability; it exists to serve when the tuners cannot.
// Picture quality outranks tuner economy: a viewer notices a standard
// definition picture, and will not notice which tuner produced it. Both devices
// report the HD flag, so the comparison is meaningful in both directions --
// an HD cable feed is preferred over a standard definition antenna one.
func rankCandidate(a, b Candidate) int {
	if n := cmp.Compare(webRank(a), webRank(b)); n != 0 {
		return n
	}
	if n := cmp.Compare(codecRank(a), codecRank(b)); n != 0 {
		return n
	}
	if n := cmp.Compare(hdRank(a), hdRank(b)); n != 0 {
		return n
	}
	if n := cmp.Compare(a.Source.Rank(), b.Source.Rank()); n != 0 {
		return n
	}
	return compareChannelNumbers(a.GuideNumber, b.GuideNumber)
}

func webRank(c Candidate) int {
	if c.Web {
		return 1
	}
	return 0
}

func codecRank(c Candidate) int {
	if compatible(c.VideoCodec) {
		return 0
	}
	return 1
}

func hdRank(c Candidate) int {
	if c.HD {
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
	return trimDigitalSuffix(b.String())
}

// trimDigitalSuffix drops a bare DT from the end of a callsign.
//
// Cable writes the high definition feed of a local station as WDIVDT, WJBKDT
// and so on, alongside a standard definition WDIV. Without this, the antenna's
// WDIV-HD matches only the standard definition cable listing, and the high
// definition one -- the channel a viewer is far more likely to be watching --
// never picks up an antenna alternative at all.
//
// Numbered variants are left alone: WDIVDT2 and WDIVDT3 are separate
// subchannels carrying different programmes, and they do not end in DT. The
// remainder must still look like a callsign, which keeps WUDT (a real station,
// listed as WUDT-LD) from being cut down to WU.
func trimDigitalSuffix(s string) string {
	const minCallsign = 3
	if rest, ok := strings.CutSuffix(s, "DT"); ok && len(rest) >= minCallsign {
		return rest
	}
	return s
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
// Several classes of channel are dropped outright rather than merely ranked
// low, because there is no circumstance in which routing a consumer to them
// helps:
//
//   - ATSC 3.0, unless opts.AllowATSC3 is set. Its AC4 audio does not play
//     reliably on the consumers. Every such channel here shadows an ATSC 1.0
//     twin that does play, so nothing is lost by dropping them.
//   - Copy-protected channels, which the consumers cannot play at all.
//   - ATSC 3.0 mobile companion feeds, which are meant for handheld devices.
//     Redundant while ATSC 3.0 is excluded wholesale, but it keeps them out if
//     next-generation channels are ever admitted.
func Merge(states []device.State, opts Options) Lineup {
	excludeSet := make(map[string]bool, len(opts.Exclude))
	for _, e := range opts.Exclude {
		excludeSet[refKey(e.Device, e.Channel)] = true
	}
	// Sources named by a mapping are attached by hand later, so they are held
	// back from automatic matching and from standing alone.
	mappedSources := make(map[string]bool, len(opts.Mappings))
	for _, m := range opts.Mappings {
		mappedSources[refKey(m.Source.Device, m.Source.Channel)] = true
	}

	var excluded Exclusions
	usable := make(map[*device.Device][]hdhr.Channel)
	byRef := make(map[string]Candidate) // every usable candidate, for mapping lookups
	var devices []*device.Device

	for _, s := range states {
		if s.Err != nil {
			continue
		}
		devices = append(devices, s.Device)
		for _, ch := range s.Lineup {
			switch {
			case excludeSet[refKey(s.Device.Name, ch.GuideNumber)]:
				excluded.Manual++
			case !opts.AllowATSC3 && isATSC3(ch):
				excluded.ATSC3++
			case ch.Protected():
				excluded.DRM++
			case mobileVariant(ch):
				excluded.Mobile++
			default:
				usable[s.Device] = append(usable[s.Device], ch)
				byRef[refKey(s.Device.Name, ch.GuideNumber)] = candidateOf(s.Device, ch)
			}
		}
	}

	spine := spineSource(devices)

	// Index the alternate devices' channels by station, so each spine channel
	// can pick up every feed of the same station. Manually mapped sources are
	// held back; they attach only where the mapping says.
	alternates := make(map[string][]Candidate)
	for _, d := range devices {
		if d.Source == spine {
			continue
		}
		for _, ch := range usable[d] {
			if mappedSources[refKey(d.Name, ch.GuideNumber)] {
				continue
			}
			if key := normalizeName(ch.GuideName); key != "" {
				alternates[key] = append(alternates[key], candidateOf(d, ch))
			}
		}
	}

	var channels []Channel
	attached := make(map[string]bool)

	// The spine's channels are the lineup, one for one. They are never merged
	// with each other: cable presents WDIV at 4 and WDIVDT at 232 as separate
	// channels with separate guide data, and Sourcery must not second-guess
	// that. Alternates attach to every spine channel they match, so an antenna
	// feed can serve both the standard and high definition cable listings.
	for _, d := range devices {
		if d.Source != spine {
			continue
		}
		for _, ch := range usable[d] {
			key := normalizeName(ch.GuideName)
			cands := append([]Candidate{candidateOf(d, ch)}, alternates[key]...)
			slices.SortStableFunc(cands, rankCandidate)
			attached[key] = true

			channels = append(channels, Channel{
				Number:     ch.GuideNumber, // identity always comes from the spine
				Name:       ch.GuideName,
				HD:         anyHD(cands),
				Candidates: cands,
				spine:      true,
			})
		}
	}

	// Alternate channels with no counterpart on the spine still belong in the
	// lineup; they are simply not available from the comprehensive source. Each
	// entry stands alone, so the antenna's seven distinct WDWO-CD subchannels
	// stay seven channels. Mapped sources are excluded here too.
	for _, d := range devices {
		if d.Source == spine {
			continue
		}
		for _, ch := range usable[d] {
			if mappedSources[refKey(d.Name, ch.GuideNumber)] {
				continue
			}
			if key := normalizeName(ch.GuideName); key == "" || attached[key] {
				continue
			}
			c := candidateOf(d, ch)
			channels = append(channels, Channel{
				Number:     ch.GuideNumber,
				Name:       ch.GuideName,
				HD:         c.HD,
				Candidates: []Candidate{c},
			})
		}
	}

	unmatchedMappings := applyMappings(channels, opts.Mappings, byRef)
	unmatchedStreams := applyStreams(channels, opts.Streams)

	slices.SortStableFunc(channels, func(a, b Channel) int {
		if n := compareChannelNumbers(a.Number, b.Number); n != 0 {
			return n
		}
		return cmp.Compare(a.Name, b.Name)
	})

	return Lineup{
		Channels:          channels,
		Excluded:          excluded,
		UnmatchedMappings: unmatchedMappings,
		UnmatchedStreams:  unmatchedStreams,
	}
}

// applyStreams attaches each configured web stream to its target channel as a
// last-resort candidate. Because a web stream ranks below every tuner, it only
// serves when the tuners for that channel are exhausted. A stream whose target
// channel is not present is returned as unmatched.
func applyStreams(channels []Channel, streams []config.Stream) []config.Stream {
	if len(streams) == 0 {
		return nil
	}

	byNumber := make(map[string]*Channel, len(channels))
	for i := range channels {
		byNumber[channels[i].Number] = &channels[i]
	}

	var unmatched []config.Stream
	for _, s := range streams {
		target, ok := byNumber[s.Channel]
		if !ok {
			unmatched = append(unmatched, s)
			continue
		}
		target.Candidates = append(target.Candidates, Candidate{
			Device:  "web",
			URL:     s.URL,
			Headers: s.Headers,
			Web:     true,
		})
		slices.SortStableFunc(target.Candidates, rankCandidate)
	}
	return unmatched
}

// applyMappings attaches each mapping's source to its target presented channel,
// re-ranking the target so the manual route competes on the same terms as the
// automatic ones. A mapping whose target channel or source is not present is
// returned as unmatched rather than being silently ignored.
func applyMappings(channels []Channel, mappings []config.Mapping, byRef map[string]Candidate) []config.Mapping {
	if len(mappings) == 0 {
		return nil
	}

	byNumber := make(map[string]*Channel, len(channels))
	for i := range channels {
		byNumber[channels[i].Number] = &channels[i]
	}

	var unmatched []config.Mapping
	for _, m := range mappings {
		target, haveTarget := byNumber[m.Channel]
		source, haveSource := byRef[refKey(m.Source.Device, m.Source.Channel)]
		if !haveTarget || !haveSource {
			unmatched = append(unmatched, m)
			continue
		}
		target.Candidates = append(target.Candidates, source)
		slices.SortStableFunc(target.Candidates, rankCandidate)
		target.HD = anyHD(target.Candidates)
	}
	return unmatched
}

// spineSource picks the source that supplies the lineup's identity.
//
// Cable wins when present: it is far more comprehensive (492 channels against
// 70 here), and it is the lineup the consumers' guide data is built around.
// Presenting cable's numbering means a consumer sees the lineup it expects
// while Sourcery quietly serves it from the antenna wherever it can.
func spineSource(devices []*device.Device) config.Source {
	for _, d := range devices {
		if d.Source == config.SourceCable {
			return config.SourceCable
		}
	}
	return config.SourceAntenna
}

func candidateOf(d *device.Device, ch hdhr.Channel) Candidate {
	return Candidate{
		Device:      d.Name,
		Source:      d.Source,
		GuideNumber: ch.GuideNumber,
		URL:         ch.URL,
		VideoCodec:  ch.VideoCodec,
		AudioCodec:  ch.AudioCodec,
		HD:          ch.HD != 0,
	}
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
