package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// CommitMetadataVersion is the current version of the metadata format
const CommitMetadataVersion = 1

// CommitMetadata is stored in commit messages as JSON
type CommitMetadata struct {
	Version     int          `json:"v"`
	Message     string       `json:"msg"`
	HLC         HLCTimestamp `json:"hlc"`
	ContentHash string       `json:"content_hash"`
	Author      string       `json:"author"`
	Email       string       `json:"email"`
	Date        time.Time    `json:"date"`
	Signature   string       `json:"sig"`
}

// CommitMetadataForSigning excludes the Signature field
type CommitMetadataForSigning struct {
	Version     int          `json:"v"`
	Message     string       `json:"msg"`
	HLC         HLCTimestamp `json:"hlc"`
	ContentHash string       `json:"content_hash"`
	Author      string       `json:"author"`
	Email       string       `json:"email"`
	Date        time.Time    `json:"date"`
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
		Message:     m.Message,
		HLC:         m.HLC,
		ContentHash: m.ContentHash,
		Author:      m.Author,
		Email:       m.Email,
		Date:        m.Date,
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
