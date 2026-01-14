package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// CommitMetadataVersion is the current version of the metadata format
const CommitMetadataVersion = 1

// Commit kinds
const (
	CommitKindNormal       = ""             // Default: regular commit
	CommitKindResubmission = "resubmission" // Resubmitted offline commit
)

// OriginEnvelope captures provenance for resubmitted commits.
// When Kind == CommitKindResubmission, this envelope is populated.
type OriginEnvelope struct {
	OriginMetadataJSON string       `json:"origin_metadata_json"`         // Original commit's metadata JSON
	OriginMetadataSig  string       `json:"origin_metadata_sig"`          // Original signature
	OriginHLC          HLCTimestamp `json:"origin_hlc"`                   // Original HLC
	OriginEventID      string       `json:"origin_event_id"`              // SHA256(OriginMetadataJSON) for idempotency
	OriginPeerID       string       `json:"origin_peer_id,omitempty"`     // Optional: original author peer ID
	OriginCommitHash   string       `json:"origin_commit_hash,omitempty"` // Optional: hash on original peer
	AuditRef           string       `json:"audit_ref,omitempty"`          // Optional: external audit reference
}

// CommitMetadata is stored in commit messages as JSON
type CommitMetadata struct {
	Version     int          `json:"v"`
	Kind        string       `json:"kind,omitempty"` // "" (normal) or "resubmission"
	Message     string       `json:"msg"`
	HLC         HLCTimestamp `json:"hlc"`
	ContentHash string       `json:"content_hash"`
	Author      string       `json:"author"`
	Email       string       `json:"email"`
	Date        time.Time    `json:"date"`
	Signature   string       `json:"sig"`

	// Present only when Kind == CommitKindResubmission
	Origin *OriginEnvelope `json:"origin,omitempty"`
}

// CommitMetadataForSigning excludes the Signature field
type CommitMetadataForSigning struct {
	Version     int             `json:"v"`
	Kind        string          `json:"kind,omitempty"`
	Message     string          `json:"msg"`
	HLC         HLCTimestamp    `json:"hlc"`
	ContentHash string          `json:"content_hash"`
	Author      string          `json:"author"`
	Email       string          `json:"email"`
	Date        time.Time       `json:"date"`
	Origin      *OriginEnvelope `json:"origin,omitempty"`
}

// NewCommitMetadata creates new metadata
func NewCommitMetadata(msg string, hlc HLCTimestamp, contentHash string, author string, email string, date time.Time) *CommitMetadata {
	return &CommitMetadata{
		Version:     CommitMetadataVersion,
		Message:     msg,
		HLC:         hlc,
		ContentHash: contentHash,
		Author:      author,
		Email:       email,
		Date:        date,
	}
}

// SignableData returns bytes to sign
func (m *CommitMetadata) SignableData() ([]byte, error) {
	forSigning := CommitMetadataForSigning{
		Version:     m.Version,
		Kind:        m.Kind,
		Message:     m.Message,
		HLC:         m.HLC,
		ContentHash: m.ContentHash,
		Author:      m.Author,
		Email:       m.Email,
		Date:        m.Date,
		Origin:      m.Origin,
	}
	return json.Marshal(forSigning)
}

// Sign signs the metadata using the provided signer
func (m *CommitMetadata) Sign(signer Signer) error {
	data, err := m.SignableData()
	if err != nil {
		return fmt.Errorf("failed to get signable data: %w", err)
	}

	signature, err := signer.Sign(string(data))
	if err != nil {
		return fmt.Errorf("failed to sign: %w", err)
	}

	m.Signature = signature
	return nil
}

// Verify verifies the metadata signature
func (m *CommitMetadata) Verify(signer Signer, publicKey string) error {
	data, err := m.SignableData()
	if err != nil {
		return fmt.Errorf("failed to get signable data: %w", err)
	}

	return signer.Verify(string(data), m.Signature, publicKey)
}

// Marshal serializes to JSON string
func (m *CommitMetadata) Marshal() (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ParseCommitMetadata parses JSON commit message
func ParseCommitMetadata(commitMessage string) (*CommitMetadata, error) {
	var m CommitMetadata
	if err := json.Unmarshal([]byte(commitMessage), &m); err != nil {
		return nil, err
	}
	if m.Version == 0 {
		return nil, fmt.Errorf("invalid metadata: missing version")
	}
	return &m, nil
}

// IsMetadataCommit checks if a commit message is a metadata-formatted commit
func IsMetadataCommit(commitMessage string) bool {
	_, err := ParseCommitMetadata(commitMessage)
	return err == nil
}

// IsResubmission returns true if this commit is a resubmission
func (m *CommitMetadata) IsResubmission() bool {
	return m.Kind == CommitKindResubmission
}

// ComputeOriginEventID computes a deterministic identifier for the origin event.
// This is used for idempotency: the same origin event should always produce the same ID.
func ComputeOriginEventID(metadataJSON string) string {
	hash := sha256.Sum256([]byte(metadataJSON))
	return hex.EncodeToString(hash[:])
}

// NewResubmissionMetadata creates metadata for a resubmission commit.
// The resubmission wraps the original commit's metadata in an origin envelope.
func NewResubmissionMetadata(
	msg string,
	hlc HLCTimestamp,
	contentHash string,
	author string,
	email string,
	date time.Time,
	originMeta *CommitMetadata,
	originCommitHash string,
) (*CommitMetadata, error) {
	originJSON, err := originMeta.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal origin metadata: %w", err)
	}

	return &CommitMetadata{
		Version:     CommitMetadataVersion,
		Kind:        CommitKindResubmission,
		Message:     msg,
		HLC:         hlc,
		ContentHash: contentHash,
		Author:      author,
		Email:       email,
		Date:        date,
		Origin: &OriginEnvelope{
			OriginMetadataJSON: originJSON,
			OriginMetadataSig:  originMeta.Signature,
			OriginHLC:          originMeta.HLC,
			OriginEventID:      ComputeOriginEventID(originJSON),
			OriginPeerID:       originMeta.HLC.PeerID,
			OriginCommitHash:   originCommitHash,
		},
	}, nil
}

// VerifyOriginEnvelope verifies the origin envelope's signature using the provided signer.
// This checks that the original metadata was validly signed.
func (o *OriginEnvelope) VerifyOriginEnvelope(signer Signer, publicKey string) error {
	if o == nil {
		return fmt.Errorf("nil origin envelope")
	}

	// Parse the original metadata
	origMeta, err := ParseCommitMetadata(o.OriginMetadataJSON)
	if err != nil {
		return fmt.Errorf("failed to parse origin metadata: %w", err)
	}

	// Verify the stored signature matches
	if origMeta.Signature != o.OriginMetadataSig {
		return fmt.Errorf("origin signature mismatch")
	}

	// Verify the origin event ID matches
	expectedEventID := ComputeOriginEventID(o.OriginMetadataJSON)
	if o.OriginEventID != expectedEventID {
		return fmt.Errorf("origin event ID mismatch")
	}

	// Verify the actual signature
	return origMeta.Verify(signer, publicKey)
}
