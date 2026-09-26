package graphql

import "context"

type responseErrorsKey struct{}
type responseErrors struct {
	query string
	items []any
}

func addResponseError(ctx context.Context, message string) {
	if state, ok := ctx.Value(responseErrorsKey{}).(*responseErrors); ok {
		for _, v := range state.items {
			if v.(map[string]any)["message"] == message {
				return
			}
		}
		state.items = append(state.items, map[string]any{"message": message, "extensions": map[string]any{"code": "CI_EVIDENCE_UNAVAILABLE"}})
	}
}
func queryRequiresCIPolicy(ctx context.Context) bool {
	state, ok := ctx.Value(responseErrorsKey{}).(*responseErrors)
	return ok && queryHasAny(state.query, "isRequired")
}
