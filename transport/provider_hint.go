package transport

import (
	"context"
	"sync"
)

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
