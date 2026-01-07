package doltswarm

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func (db *DB) getContentHash() (string, error) {
	var hash string
	err := db.sqldb.QueryRow("SELECT dolt_hashof_db('WORKING')").Scan(&hash)
	return hash, err
}

// VerifyMetadataSignature verifies the metadata signature of a CommitAd.
//
// NOTE: Remote verification requires peer public-key lookup. Until that is wired,
// remote commits are accepted to avoid false rejections.
func (db *DB) VerifyMetadataSignature(ad *CommitAd) bool {
	if ad == nil {
		return false
	}
	if ad.HLC.PeerID == db.signer.GetID() {
		meta := ad.ToMetadata()
		return meta.Verify(db.signer, db.signer.PublicKey()) == nil
	}
	return true
}

func (db *DB) CreateCommitMetadata(msg string, hlc HLCTimestamp) (*CommitMetadata, error) {
	contentHash, err := db.getContentHash()
	if err != nil {
		return nil, fmt.Errorf("failed to get content hash: %w", err)
	}

	author := db.signer.GetID()
	email := fmt.Sprintf("%s@doltswarm", author)
	commitDate := time.Now()

	metadata := NewCommitMetadata(msg, hlc, contentHash, author, email, commitDate)
	if err := metadata.Sign(db.signer); err != nil {
		return nil, fmt.Errorf("failed to sign: %w", err)
	}
	return metadata, nil
}

func doCommitWithMetadata(tx *sql.Tx, metadata *CommitMetadata, signer Signer) (string, error) {
	if signer == nil {
		return "", fmt.Errorf("no signer available")
	}

	metadataJSON, err := metadata.Marshal()
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata: %w", err)
	}

	var commitHash string
	err = tx.QueryRow(fmt.Sprintf(
		"CALL DOLT_COMMIT('-A', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		escapeSQL(metadataJSON),
		metadata.Author,
		metadata.Email,
		metadata.Date.Format(time.RFC3339Nano),
	)).Scan(&commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to run commit procedure: %w", err)
	}

	signature, err := signer.Sign(commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to sign commit '%s': %w", commitHash, err)
	}
	tagcmd := fmt.Sprintf("CALL DOLT_TAG('-m', '%s', '--author', '%s <%s>', '%s', '%s');",
		signer.PublicKey(), metadata.Author, metadata.Email, signature, commitHash)
	_, err = tx.Exec(tagcmd)
	if err != nil {
		return "", fmt.Errorf("failed to create signature tag (%s): %w", signature, err)
	}

	return commitHash, nil
}
