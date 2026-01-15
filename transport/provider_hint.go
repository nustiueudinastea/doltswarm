package transport

import (
	"context"
	"sync"
)

type preferredProviderKey struct{}
type excludedProvidersKey struct{}
type usedProviderKey struct{}

// UsedProviderTracker allows capturing which provider was actually used during a fetch.
type UsedProviderTracker struct {
	mu       sync.Mutex
	provider string
}

// Set records which provider was used.
func (t *UsedProviderTracker) Set(providerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.provider = providerID
}

// Get returns the provider that was used.
func (t *UsedProviderTracker) Get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.provider
}

// WithUsedProviderTracker returns a context with a tracker for capturing which provider was used.
func WithUsedProviderTracker(ctx context.Context, tracker *UsedProviderTracker) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, usedProviderKey{}, tracker)
}

// UsedProviderTrackerFromContext returns the tracker, if present.
func UsedProviderTrackerFromContext(ctx context.Context) *UsedProviderTracker {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(usedProviderKey{})
	t, _ := v.(*UsedProviderTracker)
	return t
}

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

// WithExcludedProviders returns a context that hints which providers should NOT be used for data-plane fetches.
// This is used to avoid retrying providers that returned stale data.
func WithExcludedProviders(ctx context.Context, providerIDs []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(providerIDs) == 0 {
		return ctx
	}
	// Merge with any existing excluded providers
	existing := ExcludedProvidersFromContext(ctx)
	merged := make(map[string]struct{}, len(existing)+len(providerIDs))
	for _, id := range existing {
		merged[id] = struct{}{}
	}
	for _, id := range providerIDs {
		merged[id] = struct{}{}
	}
	result := make([]string, 0, len(merged))
	for id := range merged {
		result = append(result, id)
	}
	return context.WithValue(ctx, excludedProvidersKey{}, result)
}

// ExcludedProvidersFromContext returns the list of providers to exclude, if present.
func ExcludedProvidersFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(excludedProvidersKey{})
	s, ok := v.([]string)
	if !ok {
		return nil
	}
	return s
}
