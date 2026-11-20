// Package turnstile detects and resolves Cloudflare Turnstile challenges.
package turnstile

import "fmt"

// State is the externally visible state of a Turnstile widget.
type State string

const (
	StateAbsent       State = "absent"
	StateSolved       State = "solved"
	StateUnresolved   State = "unresolved"
	StateInterstitial State = "interstitial"
)

const (
	ModeManaged   = "managed"
	ModeInvisible = "invisible"
	ModeUnknown   = "unknown"
)

// CostPerSolveUSD is CapSolver's Cloudflare Turnstile list price: $1.20 per
// 1,000 tasks, checked 2026-11-20.
const CostPerSolveUSD = 0.0012

// Status is what detection and resolution report. It rides on existing tool
// replies; it is not an action the caller has to take.
type Status struct {
	Detected   bool    `json:"detected"`
	Sitekey    string  `json:"sitekey,omitempty"`
	Action     string  `json:"action,omitempty"`
	CData      string  `json:"cdata,omitempty"`
	Mode       string  `json:"mode,omitempty"`
	State      State   `json:"state"`
	Widgets    int     `json:"widgets,omitempty"`
	Unsolved   int     `json:"unsolved,omitempty"`
	Attempts   int     `json:"attempts,omitempty"`
	SolverUsed bool    `json:"solverUsed,omitempty"`
	VisualUsed bool    `json:"visualUsed,omitempty"`
	VisualMS   int64   `json:"visualMs,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"` // estimated external cost: visual Jev + CapSolver
	Reason     string  `json:"reason,omitempty"`
}

// Line renders the one line added to snapshots and action replies.
func (s *Status) Line() string {
	if s == nil || !s.Detected {
		return ""
	}
	switch s.State {
	case StateSolved:
		return "TURNSTILE: solved"
	case StateInterstitial:
		return "TURNSTILE: interstitial (full-page Cloudflare challenge)"
	default:
		if s.Reason != "" {
			return "TURNSTILE: unresolved (" + s.Reason + ")"
		}
		if s.Widgets > 1 {
			return fmt.Sprintf("TURNSTILE: unresolved (%d of %d widgets)", s.Unsolved, s.Widgets)
		}
		return "TURNSTILE: unresolved"
	}
}
