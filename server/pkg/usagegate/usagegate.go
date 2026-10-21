// Package usagegate implements an account-level cooldown for provider
// subscription usage windows (Kimi Code's 5-hour rolling / weekly / monthly
// quotas are the first user; the design is provider-neutral).
//
// Why this exists: Kimi Code returns HTTP 403 with "You've reached your
// 5-hour usage limit. Your quota will reset when the current 5-hour window
// ends." when a shared account's window is exhausted. Every device and API
// key logged into that account shares the window, so two agents on one host
// (e.g. a builder and an auditor on the same login) can exhaust it together,
// and any run started before the window resets fails in seconds while
// burning nothing but dispatch noise. The gate records the exhaustion and
// fails subsequent runs on the same account FAST — before the agent CLI is
// even spawned — until the window's reset deadline passes.
//
// The 403 body carries no reset timestamp, so deadlines are conservative
// upper bounds derived from the window length measured from the observation:
// a rolling 5-hour window has fully turned over at most 5 hours after any
// observation, a weekly window at most 7 days, and a monthly cycle at most
// 31 days (the longest month). Over-blocking is therefore bounded and
// self-corrects on the next run; under-blocking is impossible. A short
// estimate would re-arm on the next 403, but a long one only delays recovery,
// so the bounds err long.
//
// Kimi's OTHER 403 — "You've reached your concurrent request limit" — is a
// risk-control concurrency rejection, not quota exhaustion, and never arms
// the gate. ParseWindow's witnesses deliberately exclude it.
package usagegate

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Window identifies which provider subscription window was exhausted.
type Window string

const (
	// Window5Hour is Kimi's 5-hour rolling quota window.
	Window5Hour Window = "5-hour"
	// Window7Day is Kimi's weekly (7-day) quota window, legacy plans only.
	Window7Day Window = "7-day"
	// WindowMonthly is Kimi's billing-cycle monthly quota window.
	WindowMonthly Window = "monthly"
)

// maxDuration returns the conservative upper bound on how long after an
// exhaustion observation the window can still be exhausted. Callers arm the
// cooldown with observedAt.Add(w.UpperBound()).
func (w Window) UpperBound() time.Duration {
	switch w {
	case Window5Hour:
		return 5 * time.Hour
	case Window7Day:
		return 7 * 24 * time.Hour
	case WindowMonthly:
		// The longest billing month; see the package doc for why the
		// bound errs long.
		return 31 * 24 * time.Hour
	}
	return 0
}

// windowWitnesses maps the lowercased official Kimi error wording onto the
// window it reports. The phrases come from Kimi's error reference
// (kimi.com/code/docs error-reference): "You've reached your 5-hour usage
// limit", "…your weekly (7-day) usage limit", "…your monthly usage limit for
// this billing cycle". Substring witnesses tolerate the CLI wrapping the
// message in "error, status code: 403, message: …" framing.
//
// Deliberately more specific than the classifier's generic "usage limit"
// witness: the gate must not arm on unrelated quota wordings (402 balance
// failures, credits exhausted) that have a different recovery story. The
// concurrent request limit 403 shares none of these substrings.
var windowWitnesses = []struct {
	witness string
	window  Window
}{
	{"5-hour usage limit", Window5Hour},
	{"5 hour usage limit", Window5Hour},
	// The official weekly wording is "weekly (7-day) usage limit"; the
	// paren-free variants cover reworded or hand-copied shapes.
	{"weekly (7-day) usage limit", Window7Day},
	{"weekly (7 day) usage limit", Window7Day},
	{"7-day usage limit", Window7Day},
	{"7 day usage limit", Window7Day},
	{"weekly usage limit", Window7Day},
	{"monthly usage limit", WindowMonthly},
}

// ParseWindow reports which subscription usage window the error text
// witnesses, per the official provider wording. The boolean is false for
// concurrency rejections, authentication failures, and everything else that
// must not arm the gate.
func ParseWindow(errText string) (Window, bool) {
	lower := strings.ToLower(errText)
	for _, w := range windowWitnesses {
		if strings.Contains(lower, w.witness) {
			return w.window, true
		}
	}
	return "", false
}

// Cooldown is one armed account-level block: the window that was exhausted
// and the estimated instant it resets.
type Cooldown struct {
	Window Window
	Until  time.Time
}

// Gate is a concurrency-safe, in-process map of account keys to armed
// cooldowns. Entries expire lazily on read; no goroutine sweeps them, so an
// idle key costs one map slot until the process restarts.
//
// Scope: per-process (per daemon). A daemon restart forgets the cooldown and
// pays one fail-fast probe run to re-arm it — the probe cannot consume
// provider quota, because the provider refuses the request the moment the
// window is exhausted. Cross-host account sharing needs a server-side gate
// and is intentionally out of scope here.
type Gate struct {
	mu    sync.Mutex
	armed map[string]Cooldown
	// now is injectable for tests; production uses time.Now.
	now func() time.Time
}

// New returns an empty Gate.
func New() *Gate {
	return &Gate{armed: make(map[string]Cooldown), now: time.Now}
}

// WithClock overrides the gate's clock. Used by tests; production keeps
// time.Now.
func (g *Gate) WithClock(now func() time.Time) *Gate {
	g.now = now
	return g
}

// Arm records that key's subscription window was observed exhausted at
// observedAt and returns the effective reset deadline. Arming again while a
// cooldown is live keeps the LATER deadline: a fresh 403 seen during a
// cooldown means the window is still (or again) exhausted, and its reset is
// no earlier than the new observation plus the window bound.
func (g *Gate) Arm(key string, window Window, observedAt time.Time) time.Time {
	if window.UpperBound() == 0 {
		return time.Time{}
	}
	until := observedAt.Add(window.UpperBound())

	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.armed[key]; ok && existing.Until.After(until) {
		return existing.Until
	}
	g.armed[key] = Cooldown{Window: window, Until: until}
	return until
}

// Blocked reports whether key is still inside its cooldown at the gate's
// current time, returning the armed cooldown when it is. Expired entries are
// discarded here — recovery is automatic once the estimated window has
// passed, with no operator action and no probe request.
func (g *Gate) Blocked(key string) (Cooldown, bool) {
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	cd, ok := g.armed[key]
	if !ok {
		return Cooldown{}, false
	}
	if !now.Before(cd.Until) {
		delete(g.armed, key)
		return Cooldown{}, false
	}
	return cd, true
}

// Clear disarms key unconditionally. Not wired to any production surface —
// it exists for operators with shell access to a debug entrypoint and for
// tests; normal recovery is the automatic expiry in Blocked.
func (g *Gate) Clear(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.armed, key)
}

// BlockMessage renders the fail-fast error for a run refused by the gate.
//
// Its wording is load-bearing on two counts. It contains "usage limit" — a
// taskfailure classifier witness — so the refused run is persisted with
// failure_reason agent_error.provider_quota_limit and the UI shows the
// quota guidance ("run out of credit") instead of the sign-in-again copy the
// raw 403's presence used to trigger. And it never contains a bare HTTP
// status code or the words that route into the auth, capacity or context
// buckets, so the classification cannot drift as the message evolves.
func BlockMessage(provider string, cd Cooldown) string {
	return fmt.Sprintf(
		"%s provider usage limit reached: the account's %s usage window is exhausted and new runs are blocked until the window resets at %s (quota cooldown, not an authentication failure)",
		provider, cd.Window, cd.Until.UTC().Format(time.RFC3339),
	)
}
