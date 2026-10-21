package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/usagegate"
)

// usageGateProvider reports whether a provider's subscription quota is
// account-scoped and observed through fail-closed 403 usage-limit rejections
// that the daemon can gate locally. Kimi Code is the first and, so far, only
// such provider: all devices and API keys logged into one Kimi account share
// its 5-hour / weekly / monthly windows.
func usageGateProvider(provider string) bool {
	return provider == "kimi"
}

// usageAccountKey derives the gate key for a task: which provider ACCOUNT the
// run will bill, not which agent or runtime requested it. Two agents sharing
// one login must map to one key — that sharing is exactly what exhausted the
// window — while separate accounts stay independent.
//
// Identity inputs, in the same resolution order the kimi backend itself uses:
// an agent's custom env wins, then the daemon's ambient environment, then the
// CLI's per-user default. KIMI_API_KEY separates API-key accounts;
// KIMI_CODE_HOME and KIMI_CONFIG_PATH separate login-state directories (the
// subscription login lives inside the home). Agents on one host with none of
// these set share the CLI's default account and therefore share one key —
// the common single-machine builder+auditor setup.
//
// Values are fingerprinted, never embedded: KIMI_API_KEY is a secret, and a
// key that must never appear in logs or error text cannot be carried raw
// even inside an unexported map key that somebody later logs.
func usageAccountKey(provider string, customEnv map[string]string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	resolve := func(name string) string {
		if v := strings.TrimSpace(customEnv[name]); v != "" {
			return v
		}
		return strings.TrimSpace(getenv(name))
	}
	parts := make([]string, 0, 3)
	for _, name := range []string{"KIMI_API_KEY", "KIMI_CODE_HOME", "KIMI_CONFIG_PATH"} {
		if v := resolve(name); v != "" {
			sum := sha256.Sum256([]byte(v))
			parts = append(parts, name+"="+hex.EncodeToString(sum[:8]))
		}
	}
	// All-empty is a real identity too: the CLI's shared default login on
	// this host. A blank suffix keeps that distinct from any explicit
	// configuration while collapsing every default-configured agent onto it.
	return provider + "|" + strings.Join(parts, ",")
}

// taskCustomEnv returns the claimed task's agent-level custom environment,
// tolerating a claim payload that expanded no agent (nil AgentData).
func taskCustomEnv(task Task) map[string]string {
	if task.Agent == nil {
		return nil
	}
	return task.Agent.CustomEnv
}

// checkUsageWindowGate refuses to start a run whose provider account is
// inside an observed quota-window cooldown. It fails the task BEFORE the
// agent CLI is spawned — a run started against an exhausted window cannot
// succeed and its provider request is pure dispatch noise — with an error
// whose text carries the estimated reset time and classifies into the quota
// failure_reason, so the task row and UI describe a cooldown instead of an
// authentication problem. Nil gate (tests, embedded builds) means no gating.
func (d *Daemon) checkUsageWindowGate(provider string, task Task) error {
	if d.usageGate == nil || !usageGateProvider(provider) {
		return nil
	}
	cd, blocked := d.usageGate.Blocked(usageAccountKey(provider, taskCustomEnv(task), os.Getenv))
	if !blocked {
		return nil
	}
	return fmt.Errorf("%s", usagegate.BlockMessage(provider, cd))
}

// observeUsageWindowFailure arms the account cooldown when a finished run's
// error text witnesses an exhausted subscription window (see ParseWindow for
// the exact wording). Concurrency 403s, auth failures and transient errors
// return without arming — only a quota window that resets on its own schedule
// is a cooldown. The observation is local to this daemon: it cannot see other
// hosts burning the same account, and it forgets on restart, where the next
// run re-arms it at the cost of one refused provider request.
func (d *Daemon) observeUsageWindowFailure(provider string, task Task, errText string, observedAt time.Time) {
	if d.usageGate == nil || !usageGateProvider(provider) {
		return
	}
	window, ok := usagegate.ParseWindow(errText)
	if !ok {
		return
	}
	key := usageAccountKey(provider, taskCustomEnv(task), os.Getenv)
	until := d.usageGate.Arm(key, window, observedAt)
	if d.logger != nil {
		d.logger.Warn("provider usage window exhausted; blocking new runs on this account until the estimated reset",
			"provider", provider,
			"window", string(window),
			"blocked_until", until.UTC().Format(time.RFC3339),
			"task", task.ID,
		)
	}
}
