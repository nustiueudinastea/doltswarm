package transport

import "context"

type preferredProviderKey struct{}

// WithPreferredProvider returns a context that hints which provider should be used for data-plane fetches.
// Transport implementations may choose to honor or ignore this hint.
func WithPreferredProvider(ctx context.Context, providerID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if providerID == "" {
		return ctx
	}
	return context.WithValue(ctx, preferredProviderKey{}, providerID)
}

// PreferredProviderFromContext returns the preferred provider hint, if present.
func PreferredProviderFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v := ctx.Value(preferredProviderKey{})
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}
