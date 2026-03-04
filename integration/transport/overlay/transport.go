package overlay

import "github.com/nustiueudinastea/doltswarm"

// Transport is a simple composition of a Gossip control-plane and a provider-agnostic data-plane.
// This is integration-only glue; the core library should not assume any specific transport stack.
type Transport struct {
	G doltswarm.Gossip
	P doltswarm.ProviderPicker
}

func (t *Transport) Gossip() doltswarm.Gossip            { return t.G }
func (t *Transport) Providers() doltswarm.ProviderPicker { return t.P }
