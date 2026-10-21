package usagegate

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// kimi5h403 is the official wording from Kimi's error reference, wrapped the
// way the CLI surfaces it. This is the shape the AGEN-60 incident actually
// produced.
const kimi5h403 = `error, status code: 403, message: You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota`

const kimi7d403 = `error, status code: 403, message: You've reached your weekly (7-day) usage limit. Your quota will reset when the current 7-day window ends.`

const kimiMonthly403 = `error, status code: 403, message: You've reached your monthly usage limit for this billing cycle. Your quota will be refreshed in the next cycle.`

const kimiConcurrent403 = `error, status code: 403, message: You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again.`

func TestParseWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want Window
		ok   bool
	}{
		{"kimi 5h", kimi5h403, Window5Hour, true},
		{"kimi weekly", kimi7d403, Window7Day, true},
		{"kimi monthly", kimiMonthly403, WindowMonthly, true},
		{"case insensitive", `API Error: 403 You've Reached Your 5-Hour Usage Limit.`, Window5Hour, true},
		// The concurrency 403 shares the "You've reached your …" prefix and
		// the status code with the quota rejections but is a risk-control
		// rejection with a different remedy. It must never arm the gate.
		{"kimi concurrent request limit is not a window", kimiConcurrent403, "", false},
		{"auth failure is not a window", "401 The API Key appears to be invalid or may have expired.", "", false},
		{"balance failure is not a window", "402 We're unable to verify your membership benefits at this time.", "", false},
		{"rate limit is not a window", "429 We're receiving too many requests at the moment.", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseWindow(c.in)
			if got != c.want || ok != c.ok {
				t.Fatalf("ParseWindow(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestGateBlockAndAutoRecovery(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 30, 5, 24, 17, 0, time.UTC) // the incident's failure instant
	now := base
	g := New().WithClock(func() time.Time { return now })

	observed := base.Add(-time.Minute)
	until := g.Arm("kimi|default", Window5Hour, observed)
	wantUntil := observed.Add(5 * time.Hour)
	if !until.Equal(wantUntil) {
		t.Fatalf("Arm returned %v, want %v", until, wantUntil)
	}

	if _, blocked := g.Blocked("kimi|default"); !blocked {
		t.Fatal("gate must block immediately after arming — the incident's 2-minute re-dispatch is exactly this window")
	}

	// One minute later (the incident's re-dispatch): still blocked.
	now = base
	if _, blocked := g.Blocked("kimi|default"); !blocked {
		t.Fatal("gate must still block one minute after the 403")
	}

	// A different account is independent: the cooldown is account-scoped,
	// not global, so a second Kimi account on the same host keeps running.
	if _, blocked := g.Blocked("kimi|KIMI_API_KEY=abc"); blocked {
		t.Fatal("gate must not block a different account key")
	}

	// At the estimated reset the cooldown lifts by itself — no probe
	// request, no operator action.
	now = wantUntil
	if _, blocked := g.Blocked("kimi|default"); blocked {
		t.Fatal("gate must auto-recover once the estimated window has passed")
	}

	// And it stays lifted afterwards.
	now = wantUntil.Add(time.Hour)
	if _, blocked := g.Blocked("kimi|default"); blocked {
		t.Fatal("gate must stay recovered after the reset deadline")
	}
}

func TestGateRearmKeepsLaterDeadline(t *testing.T) {
	t.Parallel()

	g := New()
	first := time.Date(2026, 9, 30, 5, 0, 0, 0, time.UTC)
	// First observation arms until 10:00.
	g.Arm("k", Window5Hour, first)
	// A fresh 403 seen at 08:00 (the window refilled, was burned again and
	// re-exhausted) means the reset is no earlier than 13:00 — arming must
	// not shorten the cooldown back to 10:00.
	until := g.Arm("k", Window5Hour, first.Add(3*time.Hour))
	want := first.Add(3 * time.Hour).Add(5 * time.Hour)
	if !until.Equal(want) {
		t.Fatalf("re-arm returned %v, want the later deadline %v", until, want)
	}
}

func TestGateClear(t *testing.T) {
	t.Parallel()

	g := New()
	g.Arm("k", WindowMonthly, time.Now())
	g.Clear("k")
	if _, blocked := g.Blocked("k"); blocked {
		t.Fatal("Clear must disarm the key")
	}
}

func TestArmUnknownWindowIsNoop(t *testing.T) {
	t.Parallel()

	g := New()
	if until := g.Arm("k", Window("bogus"), time.Now()); !until.IsZero() {
		t.Fatalf("Arm with an unknown window must return the zero time, got %v", until)
	}
	if _, blocked := g.Blocked("k"); blocked {
		t.Fatal("an unknown window must not arm the gate")
	}
}

// TestBlockMessageClassifiesAsQuota pins the fail-fast error onto the quota
// failure_reason. The message is the only thing the server sees for a
// gate-refused run, so if its wording drifts into another bucket the task row
// and UI would misdescribe the cooldown — the exact "Authentication required"
// failure mode this gate exists to end.
func TestBlockMessageClassifiesAsQuota(t *testing.T) {
	t.Parallel()

	for _, w := range []Window{Window5Hour, Window7Day, WindowMonthly} {
		msg := BlockMessage("kimi", Cooldown{Window: w, Until: time.Date(2026, 9, 30, 10, 16, 59, 0, time.UTC)})
		if got := taskfailure.Classify(msg); got != taskfailure.ReasonAgentProviderQuotaLimit {
			t.Errorf("Classify(BlockMessage(%s)) = %s, want %s (message: %q)", w, got, taskfailure.ReasonAgentProviderQuotaLimit, msg)
		}
		if !strings.Contains(msg, "usage limit") {
			t.Errorf("BlockMessage(%s) lost the classifier witness: %q", w, msg)
		}
	}
}
