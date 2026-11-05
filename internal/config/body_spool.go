package config

// BodySpoolConfig configures the skeleton + blob request-body spool (large-request-capacity
// D2-D4). Large media strings of covered request bodies are kept in an anonymous on-disk spool and
// spliced back at the codex, claude and openai-compat send points. Disabled by default.
type BodySpoolConfig struct {
	// Enabled turns the spool on. Startup fails when Dir is not a valid spool directory.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Dir is the spool directory: a disk volume (not tmpfs or ramfs), mode 0700, O_TMPFILE capable.
	Dir string `yaml:"dir" json:"dir"`
	// LedgerDir is the read-only capacity ledger mount. Without it, bodies of 64 MiB or more are refused.
	LedgerDir string `yaml:"ledger-dir" json:"ledger-dir"`
	// ThresholdBytes (T) is the smallest allow-listed media string that is spooled. Default 64 KiB.
	ThresholdBytes int `yaml:"threshold-bytes" json:"threshold-bytes"`
	// InlineLimitBytes (S) bounds the inline bytes of every covered request. Default 16 MiB.
	InlineLimitBytes int64 `yaml:"inline-limit-bytes" json:"inline-limit-bytes"`
	// TicketedInlineLimitBytes bounds the inline bytes of a ticketed request. Default 8 MiB.
	TicketedInlineLimitBytes int64 `yaml:"ticketed-inline-limit-bytes" json:"ticketed-inline-limit-bytes"`
	// FallbackLimitBytes (L) bounds bodies on paths that are not converted. Default and maximum 16 MiB.
	FallbackLimitBytes int64 `yaml:"fallback-limit-bytes" json:"fallback-limit-bytes"`
	// DiskReserveBytes is the free space every spool write keeps. Default 1 GiB.
	DiskReserveBytes int64 `yaml:"disk-reserve-bytes" json:"disk-reserve-bytes"`
	// CgroupDir holds memory.max and memory.current for the per-hop memory charge. Default /sys/fs/cgroup.
	CgroupDir string `yaml:"cgroup-dir" json:"cgroup-dir"`
	// KSkel is the measured skeleton memory multiple charged per inline byte. Default 9.
	KSkel float64 `yaml:"k-skel" json:"k-skel"`
	// KMem is the anonymous growth per body byte charged for a ticketed request. Default 0.1.
	KMem float64 `yaml:"k-mem" json:"k-mem"`
	// KPath is the memory multiple charged per body byte on a fallback path. Default 9.
	KPath float64 `yaml:"k-path" json:"k-path"`
}

// Body spool defaults (design D2-D4).
const (
	DefaultBodySpoolThreshold      = 64 << 10
	DefaultBodySpoolInlineLimit    = 16 << 20
	DefaultBodySpoolTicketedInline = 8 << 20
	MaxBodySpoolFallbackLimit      = 16 << 20
	DefaultBodySpoolDiskReserve    = 1 << 30
)

// Effective returns the configuration with defaults applied and L clamped to 16 MiB.
func (c BodySpoolConfig) Effective() BodySpoolConfig {
	if c.ThresholdBytes <= 0 {
		c.ThresholdBytes = DefaultBodySpoolThreshold
	}
	if c.InlineLimitBytes <= 0 {
		c.InlineLimitBytes = DefaultBodySpoolInlineLimit
	}
	if c.TicketedInlineLimitBytes <= 0 {
		c.TicketedInlineLimitBytes = DefaultBodySpoolTicketedInline
	}
	if c.FallbackLimitBytes <= 0 || c.FallbackLimitBytes > MaxBodySpoolFallbackLimit {
		c.FallbackLimitBytes = MaxBodySpoolFallbackLimit
	}
	if c.DiskReserveBytes <= 0 {
		c.DiskReserveBytes = DefaultBodySpoolDiskReserve
	}
	if c.CgroupDir == "" {
		c.CgroupDir = "/sys/fs/cgroup"
	}
	if c.KSkel <= 0 {
		c.KSkel = 9
	}
	if c.KMem <= 0 {
		c.KMem = 0.1
	}
	if c.KPath <= 0 {
		c.KPath = 9
	}
	return c
}
