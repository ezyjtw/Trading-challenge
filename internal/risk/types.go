package risk

// Mode represents the risk engine operating mode.
// Escalation path: RUNNING -> PAUSED -> SAFE -> FLATTEN -> HALTED
// No automatic de-escalation; operator must explicitly clear.
type Mode int

const (
	ModeRunning Mode = iota
	ModePaused
	ModeSafe
	ModeFlatten
	ModeHalted
)

// String returns the human-readable name for a Mode.
func (m Mode) String() string {
	switch m {
	case ModeRunning:
		return "RUNNING"
	case ModePaused:
		return "PAUSED"
	case ModeSafe:
		return "SAFE"
	case ModeFlatten:
		return "FLATTEN"
	case ModeHalted:
		return "HALTED"
	default:
		return "UNKNOWN"
	}
}

// ParseMode converts a string into a Mode value. Returns ModeRunning if unrecognised.
func ParseMode(s string) Mode {
	switch s {
	case "RUNNING":
		return ModeRunning
	case "PAUSED":
		return ModePaused
	case "SAFE":
		return ModeSafe
	case "FLATTEN":
		return ModeFlatten
	case "HALTED":
		return ModeHalted
	default:
		return ModeRunning
	}
}

// Severity represents the severity level of an alert.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarn
	SeverityCritical
)

// String returns the human-readable name for a Severity.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "INFO"
	case SeverityWarn:
		return "WARN"
	case SeverityCritical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

// State holds the current risk engine state. No TenantID — single-exchange challenge mode.
type State struct {
	Mode             Mode   `json:"mode"`
	TsMs             int64  `json:"ts_ms"`
	DrawdownPct      float64 `json:"drawdown_pct"`
	DailyDrawdownPct float64 `json:"daily_drawdown_pct"`
	PeakEquityUSD    float64 `json:"peak_equity_usd"`
	CurrentEquityUSD float64 `json:"current_equity_usd"`
	ErrorRate5mPct   float64 `json:"error_rate_5m_pct"`
	Reason           string  `json:"reason"`
	ChallengePhase   string  `json:"challenge_phase"`
}

// Alert represents a risk alert emitted by the daemon.
type Alert struct {
	TsMs      int64    `json:"ts_ms"`
	Source    string   `json:"source"`
	Severity  Severity `json:"severity"`
	Message   string   `json:"message"`
	Metric    string   `json:"metric"`
	Value     float64  `json:"value"`
	Threshold float64  `json:"threshold"`
}
