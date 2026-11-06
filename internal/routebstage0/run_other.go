//go:build !linux

package routebstage0

// Darwin admission requires its separately versioned hardened derivation and
// sealed-system/shared-cache contract. A Linux file table cannot stand in for it.
func Run(id, operation string) error { return refused("stage0_platform_authority_required") }
