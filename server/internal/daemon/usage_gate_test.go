package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
	"github.com/multica-ai/multica/server/pkg/usagegate"
)

// newUsageGateTestDaemon builds the minimal Daemon the gate helpers touch.
// Everything else stays zero so a nil-unsafe dependency fails loudly here
// rather than in production wiring.
func newUsageGateTestDaemon(t *testing.T, now func() time.Time) *Daemon {
	t.Helper()
	return &Daemon{usageGate: usagegate.New().WithClock(now)}
}

func kimiTask(customEnv map[string]string) Task {
	task := Task{ID: "t1", WorkspaceID: "ws"}
	if customEnv != nil {
		task.Agent = &AgentData{Name: "kimi-builder", CustomEnv: customEnv}
	}
	return task
}

// TestUsageAccountKeySharedLogin verifies the account identity the incident
// exposed: two agents on one host with no explicit Kimi configuration share
// the CLI's default login, must share one gate key, and must stay separable
// from agents configured with their own credentials.
func TestUsageAccountKeySharedLogin(t *testing.T) {
	t.Parallel()

	getenv := func(string) string { return "" }

	shared1 := usageAccountKey("kimi", nil, getenv)
	shared2 := usageAccountKey("kimi", map[string]string{"OTHER": "x"}, getenv)
	if shared1 != shared2 {
		t.Fatalf("default-login agents must share one key: %q vs %q", shared1, shared2)
	}

	keyed := usageAccountKey("kimi", map[string]string{"KIMI_API_KEY": "sk-a"}, getenv)
	separate := usageAccountKey("kimi", map[string]string{"KIMI_API_KEY": "sk-b"}, getenv)
	if keyed == shared1 || separate == shared1 || keyed == separate {
		t.Fatal("distinct credentials must produce distinct keys")
	}

	// An agent-level KIMI_CODE_HOME wins over the ambient one, mirroring the
	// backend's env layering.
	ambient := func(name string) string {
		if name == "KIMI_CODE_HOME" {
			return "/ambient"
		}
		return ""
	}
	if usageAccountKey("kimi", map[string]string{"KIMI_CODE_HOME": "/agent"}, ambient) ==
		usageAccountKey("kimi", nil, ambient) {
		t.Fatal("agent-level KIMI_CODE_HOME must override the ambient value in the key")
	}

	// The secret must never be embedded verbatim.
	if strings.Contains(keyed, "sk-a") {
		t.Fatalf("key leaks the raw API key: %q", keyed)
	}
}

// TestCheckUsageWindowGate exercises the pre-run refusal: blocked accounts
// fail fast with a quota-classified, reset-time-carrying error; everything
// else — other providers, unblocked accounts, nil gate — passes through.
func TestCheckUsageWindowGate(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 30, 5, 24, 17, 0, time.UTC)
	now := base
	d := newUsageGateTestDaemon(t, func() time.Time { return now })
	observed := base.Add(-time.Minute)
	d.usageGate.Arm("kimi|", usagegate.Window5Hour, observed)

	err := d.checkUsageWindowGate("kimi", kimiTask(nil))
	if err == nil {
		t.Fatal("a kimi task on the cooled-down default account must be refused")
	}
	if got := taskfailure.Classify(err.Error()); got != taskfailure.ReasonAgentProviderQuotaLimit {
		t.Fatalf("refusal classifies as %s, want %s (text: %q)", got, taskfailure.ReasonAgentProviderQuotaLimit, err.Error())
	}
	if !strings.Contains(err.Error(), observed.Add(5*time.Hour).UTC().Format(time.RFC3339)) {
		t.Fatalf("refusal must state the estimated reset time: %q", err.Error())
	}

	// A different provider is never gated by the kimi cooldown.
	if err := d.checkUsageWindowGate("claude", kimiTask(nil)); err != nil {
		t.Fatalf("non-kimi provider must not be gated, got %v", err)
	}
	// A kimi agent with its own API key is a different account.
	if err := d.checkUsageWindowGate("kimi", kimiTask(map[string]string{"KIMI_API_KEY": "sk-other"})); err != nil {
		t.Fatalf("a separate account must not be gated, got %v", err)
	}

	// After the estimated reset the same task passes.
	now = base.Add(5 * time.Hour)
	if err := d.checkUsageWindowGate("kimi", kimiTask(nil)); err != nil {
		t.Fatalf("gate must auto-recover after the reset, got %v", err)
	}

	// A nil gate (embedded/test daemons) never blocks.
	bare := &Daemon{}
	if err := bare.checkUsageWindowGate("kimi", kimiTask(nil)); err != nil {
		t.Fatalf("nil gate must disable gating, got %v", err)
	}
}

// TestObserveUsageWindowFailure pins which failures arm the cooldown: the
// official window witnesses do; the concurrency 403 and everything else do
// not.
func TestObserveUsageWindowFailure(t *testing.T) {
	t.Parallel()

	observed := time.Date(2026, 9, 30, 5, 24, 17, 0, time.UTC)

	t.Run("kimi 5h arms the shared default account", func(t *testing.T) {
		d := newUsageGateTestDaemon(t, func() time.Time { return observed.Add(time.Minute) })
		d.observeUsageWindowFailure("kimi", kimiTask(nil),
			`error, status code: 403, message: You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends.`, observed)
		if err := d.checkUsageWindowGate("kimi", kimiTask(nil)); err == nil {
			t.Fatal("5h witness must arm the gate for the same account")
		}
		// A second agent sharing the login is blocked too — the account is
		// the key, not the agent.
		otherAgent := Task{ID: "t2", WorkspaceID: "ws", Agent: &AgentData{Name: "kimi-auditor"}}
		if err := d.checkUsageWindowGate("kimi", otherAgent); err == nil {
			t.Fatal("a second agent on the shared login must be blocked by the first agent's 403")
		}
	})

	t.Run("kimi concurrency 403 does not arm", func(t *testing.T) {
		d := newUsageGateTestDaemon(t, func() time.Time { return observed })
		d.observeUsageWindowFailure("kimi", kimiTask(nil),
			`error, status code: 403, message: You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again.`, observed)
		if err := d.checkUsageWindowGate("kimi", kimiTask(nil)); err != nil {
			t.Fatalf("concurrency 403 must not arm the quota gate, got %v", err)
		}
	})

	t.Run("other providers and texts do not arm", func(t *testing.T) {
		d := newUsageGateTestDaemon(t, func() time.Time { return observed })
		d.observeUsageWindowFailure("claude", kimiTask(nil), `403 You've reached your 5-hour usage limit.`, observed)
		if err := d.checkUsageWindowGate("claude", kimiTask(nil)); err != nil {
			t.Fatalf("non-kimi provider must not arm, got %v", err)
		}
		d.observeUsageWindowFailure("kimi", kimiTask(nil), "kimi initialize failed: connection refused", observed)
		if err := d.checkUsageWindowGate("kimi", kimiTask(nil)); err != nil {
			t.Fatalf("unrelated kimi failure must not arm, got %v", err)
		}
	})

	t.Run("nil gate is a no-op", func(t *testing.T) {
		bare := &Daemon{}
		bare.observeUsageWindowFailure("kimi", kimiTask(nil), `403 You've reached your 5-hour usage limit.`, observed)
	})
}

// TestUsageGateAmbientEnvEntersKey makes sure ambient daemon environment
// variables participate in the key when the agent sets none — the
// KIMI_CODE_HOME relocation kimiSessionRoot also honours. Uses a fake getenv
// rather than t.Setenv so the suite stays parallel-safe.
func TestUsageGateAmbientEnvEntersKey(t *testing.T) {
	t.Parallel()

	ambient := func(name string) string {
		if name == "KIMI_CODE_HOME" {
			return "/ambient/kimi-home"
		}
		return ""
	}
	if usageAccountKey("kimi", nil, ambient) == "kimi|" {
		t.Fatal("an ambient KIMI_CODE_HOME must enter the account key")
	}
	if usageAccountKey("kimi", nil, ambient) != usageAccountKey("kimi", map[string]string{"KIMI_CODE_HOME": "/ambient/kimi-home"}, ambient) {
		t.Fatal("the same home via agent env and ambient env must be one account")
	}
}
