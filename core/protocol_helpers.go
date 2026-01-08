package core

import (
	"time"

	"github.com/nustiueudinastea/doltswarm/protocol"
)

// Re-export commonly-used protocol helpers so core can stay concise.

const CommitMetadataVersion = protocol.CommitMetadataVersion

func NewCommitMetadata(msg string, hlc HLCTimestamp, contentHash, author, email string, date time.Time) *CommitMetadata {
	return protocol.NewCommitMetadata(msg, hlc, contentHash, author, email, date)
}

func NewHLC(peerID string) *HLC { return protocol.NewHLC(peerID) }

func ParseCommitMetadata(commitMessage string) (*CommitMetadata, error) {
	return protocol.ParseCommitMetadata(commitMessage)
}

func IsMetadataCommit(commitMessage string) bool {
	return protocol.IsMetadataCommit(commitMessage)
}
