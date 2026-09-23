package edgeprotocol

import (
	"errors"
	"strings"
	"time"
)

const (
	RegistrationVersion        = "ags.replication.registration.v1"
	RegistrationRequestVersion = "ags.replication.register.v1"
)

// Registration is an admin-only observation, NOT a peer grant. The primary
// chooses AuthorityID and the stable store incarnation. Reading this receipt
// never provisions storage or gives a node access to any repository.
type Registration struct {
	Version      string              `json:"version"`
	AuthorityID  string              `json:"authority_id"`
	Repository   string              `json:"repository"`
	RepositoryID uint64              `json:"repository_id"`
	CreatedAt    time.Time           `json:"created_at"`
	Identity     *RepositoryIdentity `json:"identity,omitempty"`
}

// RegisterRepository binds explicit operator intent to a previously observed
// row, including creation time in case a DB recycles a numeric repository ID.
// It does not contain a caller-selected authority, store ID, or node grant.
type RegisterRepository struct {
	Version              string    `json:"version"`
	ExpectedAuthorityID  string    `json:"expected_authority_id"`
	ExpectedRepositoryID uint64    `json:"expected_repository_id"`
	ExpectedCreatedAt    time.Time `json:"expected_created_at"`
}

func ValidRepositoryLocator(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || len(value) > 512 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return false
		}
	}
	return true
}

func (r Registration) Validate() error {
	if r.Version != RegistrationVersion || !identifier(r.AuthorityID) || !ValidRepositoryLocator(r.Repository) || r.RepositoryID == 0 || r.CreatedAt.IsZero() {
		return errors.New("invalid replication registration")
	}
	if r.Identity != nil && (r.Identity.Validate() != nil || r.Identity.AuthorityID != r.AuthorityID || r.Identity.RepositoryID != r.RepositoryID || r.Identity.Kind != "repo") {
		return errors.New("invalid registration identity binding")
	}
	return nil
}

func (r RegisterRepository) Validate() error {
	if r.Version != RegistrationRequestVersion || !identifier(r.ExpectedAuthorityID) || r.ExpectedRepositoryID == 0 || r.ExpectedCreatedAt.IsZero() {
		return errors.New("registration requires exact prior repository observation")
	}
	return nil
}

func (r Registration) Request() RegisterRepository {
	return RegisterRepository{Version: RegistrationRequestVersion, ExpectedAuthorityID: r.AuthorityID, ExpectedRepositoryID: r.RepositoryID, ExpectedCreatedAt: r.CreatedAt}
}
