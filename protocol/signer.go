package protocol

// Signer signs and verifies commit metadata.
//
// Implementations are expected to provide stable peer identity via GetID().
type Signer interface {
	Sign(commit string) (string, error)
	Verify(commit string, signature string, publicKey string) error
	PublicKey() string
	GetID() string
}
