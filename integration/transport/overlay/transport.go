package overlay

import "github.com/nustiueudinastea/doltswarm"

// PeerConnector is the interface for proactive connection establishment.
// This matches doltswarm.PeerConnector but is defined here to avoid import cycles.
type PeerConnector interface {
	EnsurePeerConnected(peerID string)
}

// PeerConnectivity is the interface for checking whether a peer is connected.
// This matches doltswarm.PeerConnectivity but is defined here to avoid import cycles.
type PeerConnectivity interface {
	IsPeerConnected(peerID string) bool
}

// Transport is a simple composition of a Gossip control-plane and a provider-agnostic data-plane.
// This is integration-only glue; the core library should not assume any specific transport stack.
type Transport struct {
	G doltswarm.Gossip
	P doltswarm.ProviderPicker
	C PeerConnector // Optional: for proactive peer connection
	K PeerConnectivity
}

func (t *Transport) Gossip() doltswarm.Gossip            { return t.G }
func (t *Transport) Providers() doltswarm.ProviderPicker { return t.P }

// EnsurePeerConnected implements doltswarm.PeerConnector by delegating to the underlying connector.
func (t *Transport) EnsurePeerConnected(peerID string) {
	if t.C != nil {
		t.C.EnsurePeerConnected(peerID)
	}
}

func (t *Transport) IsPeerConnected(peerID string) bool {
	if t.K != nil {
		return t.K.IsPeerConnected(peerID)
	}
	return false
}
