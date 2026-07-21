package hdhr

// Discover is the response from a device's /discover.json.
type Discover struct {
	FriendlyName      string `json:"FriendlyName"`
	ModelNumber       string `json:"ModelNumber"`
	FirmwareName      string `json:"FirmwareName"`
	FirmwareVersion   string `json:"FirmwareVersion"`
	DeviceID          string `json:"DeviceID"`
	DeviceAuth        string `json:"DeviceAuth"`
	BaseURL           string `json:"BaseURL"`
	LineupURL         string `json:"LineupURL"`
	TunerCount        int    `json:"TunerCount"`
	ConditionalAccess int    `json:"ConditionalAccess"`
}

// Channel is one entry from a device's /lineup.json.
//
// HD, DRM, SignalStrength and SignalQuality are absent on some entries and on
// some device models; zero means "not reported" as much as it means "no".
type Channel struct {
	GuideNumber    string `json:"GuideNumber"`
	GuideName      string `json:"GuideName"`
	VideoCodec     string `json:"VideoCodec"`
	AudioCodec     string `json:"AudioCodec"`
	URL            string `json:"URL"`
	HD             int    `json:"HD,omitempty"`
	DRM            int    `json:"DRM,omitempty"`
	SignalStrength int    `json:"SignalStrength,omitempty"`
	SignalQuality  int    `json:"SignalQuality,omitempty"`
}

// Protected reports whether the channel is copy-protected, and therefore
// unplayable by the downstream consumers.
func (c Channel) Protected() bool { return c.DRM != 0 }

// Tuner is one entry from a device's /status.json.
//
// An idle tuner reports only Resource; every other field is omitted. Note that
// VctNumber is what the tuned stream advertises about itself, which is not
// necessarily the GuideNumber that was requested to reach it -- do not use it
// to identify which stream is holding a tuner.
type Tuner struct {
	Resource              string `json:"Resource"`
	VctNumber             string `json:"VctNumber,omitempty"`
	VctName               string `json:"VctName,omitempty"`
	Frequency             int64  `json:"Frequency,omitempty"`
	SignalStrengthPercent int    `json:"SignalStrengthPercent,omitempty"`
	SignalQualityPercent  int    `json:"SignalQualityPercent,omitempty"`
	SymbolQualityPercent  int    `json:"SymbolQualityPercent,omitempty"`
	TargetIP              string `json:"TargetIP,omitempty"`
}

// Active reports whether the tuner is currently tuned to something. Frequency
// is the most reliable signal: it is present on every active tuner and absent
// on every idle one.
func (t Tuner) Active() bool { return t.Frequency != 0 }

// LineupStatus is the response from a device's /lineup_status.json.
type LineupStatus struct {
	ScanInProgress int      `json:"ScanInProgress"`
	ScanPossible   int      `json:"ScanPossible"`
	Source         string   `json:"Source"`
	SourceList     []string `json:"SourceList"`
}
