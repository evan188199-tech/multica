package quotagroup

import (
	"encoding/json"
	"regexp"
	"strings"
)

var validKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// FromRuntimeConfig reads a non-secret account alias from an agent's saved
// runtime configuration. Empty or malformed configuration opts out of gating.
func FromRuntimeConfig(raw []byte) string {
	var config struct {
		QuotaGroup string `json:"quota_group"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return ""
	}
	key := strings.TrimSpace(config.QuotaGroup)
	if !validKey.MatchString(key) {
		return ""
	}
	return key
}
