package artifactretirement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ReferenceSnapshotSHA256 is the canonical reference identity of a verified
// recovery copy. The deployment owner compares archive restoration with the
// stopped source and binds this digest into the private intent. Startup refuses
// a newer/different source graph rather than destroying unarchived history.
func ReferenceSnapshotSHA256(ctx context.Context, gitDir string) (string, error) {
	refs, err := refMap(ctx, gitRunner{dir: gitDir}, true)
	if err != nil {
		return "", err
	}
	return refsSHA256(refs), nil
}

func refsSHA256(refs map[string]string) string {
	data, _ := json.Marshal(refs)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
