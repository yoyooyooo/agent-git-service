package config

import (
	"errors"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func validateReplicationAuthority(cfg Config) error {
	if cfg.ReplicationAuthorityID == "" {
		return nil
	}
	probe := edgeprotocol.RepositoryIdentity{AuthorityID: cfg.ReplicationAuthorityID, StoreID: "config", RepositoryID: 1, Kind: "repo"}
	if probe.Validate() != nil {
		return errors.New("invalid AGS_REPLICATION_AUTHORITY_ID")
	}
	if cfg.ControlPlaneDSN != "" || cfg.AllowAnyToken {
		return errors.New("replication registration requires strict single-DB native authentication")
	}
	return nil
}
