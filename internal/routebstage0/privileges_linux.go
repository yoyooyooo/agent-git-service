//go:build linux

package routebstage0

import (
	"strconv"
	"strings"
)

// UID 0 alone does not establish global /proc visibility. The root writer
// observer retains the effective ptrace and DAC capability needed to inspect
// other users, while the fixed runtime child retains no capability set.
func privilegePolicy(raw []byte, root bool) (string, error) {
	wanted := map[string]bool{"CapEff": true, "CapPrm": true, "CapInh": true, "CapAmb": true, "NoNewPrivs": true}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !wanted[key] {
			continue
		}
		if _, exists := values[key]; exists {
			return "", refused("stage0_privileges_unknown")
		}
		values[key] = strings.TrimSpace(value)
	}
	if len(values) != len(wanted) || values["NoNewPrivs"] != "1" {
		return "", refused("stage0_no_new_privileges_required")
	}
	for _, key := range []string{"CapEff", "CapPrm", "CapInh", "CapAmb"} {
		bits, err := strconv.ParseUint(values[key], 16, 64)
		if err != nil || len(values[key]) != 16 {
			return "", refused("stage0_privileges_unknown")
		}
		if !root && bits != 0 {
			return "", refused("stage0_child_capabilities_forbidden")
		}
		if root && (key == "CapEff" || key == "CapPrm") && (bits&(1<<19) == 0 || bits&((1<<1)|(1<<2)) == 0) {
			return "", refused("stage0_global_writer_visibility_required")
		}
		if root && (key == "CapAmb" || key == "CapInh") && bits != 0 {
			return "", refused("stage0_ambient_capabilities_forbidden")
		}
	}
	return values["CapEff"] + ":" + values["CapPrm"] + ":" + values["CapInh"] + ":" + values["CapAmb"] + ":" + values["NoNewPrivs"], nil
}
