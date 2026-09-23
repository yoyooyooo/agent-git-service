// Package tenant preserves only a legacy context marker for fail-closed fork
// adapters. It does not restore the retired upstream tenant database router or
// grant access to a tenant. Replication rejects every marked context.
package tenant

import "context"

type tenantKey struct{}

// ContextWithTenant marks a legacy caller context. New single-DB runtime code
// must not use this marker to choose a database or filesystem path.
func ContextWithTenant(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantKey{}, id)
}

func FromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(tenantKey{}).(string)
	return value, ok && value != ""
}
