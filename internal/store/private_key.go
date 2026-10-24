package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

func (s *Store) keyVerifier() []byte {
	mac := hmac.New(sha256.New, s.lookupKey)
	_, _ = mac.Write([]byte(privateFrame("tradewinds", "private-evidence", "v1", "store-key-verifier")))
	return mac.Sum(nil)
}

// The singleton is created only by Goose version 5. Never regenerate a missing
// verifier on startup: doing so could make a wrong key hide existing missions.
// Account credential corruption remains local to that account after cutover.
func (s *Store) verifyKey(db *gorm.DB) error {
	var row struct {
		Verifier []byte
	}
	if err := db.Table("private_evidence_key").Where("singleton = true").Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("store key verifier is missing; restore the protected database backup")
		}
		return fmt.Errorf("read store key verifier: %w", databaseError{cause: err})
	}
	if !hmac.Equal(row.Verifier, s.keyVerifier()) {
		return errors.New("store key verification failed; verify CREDENTIAL_KEY or restore the protected database backup")
	}
	return nil
}
