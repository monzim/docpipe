package httpx

import "context"

// Context key type unique to this package — prevents collisions across libraries
// that also store values in the request context.
type ctxKey int

const (
	ctxRequestID ctxKey = iota + 1
	ctxAPIKeyName
)

// WithRequestID attaches a request ID to ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestID extracts the request ID from ctx, or "" if absent.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// WithAPIKeyName attaches the authenticated API key name (not the secret) to ctx.
func WithAPIKeyName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxAPIKeyName, name)
}

// APIKeyName extracts the authenticated key name, or "" if unauthenticated.
func APIKeyName(ctx context.Context) string {
	v, _ := ctx.Value(ctxAPIKeyName).(string)
	return v
}
