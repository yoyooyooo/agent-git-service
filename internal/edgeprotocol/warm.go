package edgeprotocol

import "time"

const (
	NodeStatusPath = "/_ags/edge/v1/status"
	WarmPath       = "/_ags/edge/v1/warm"
	NodeVersion    = "ags.edge.node.v1"
)

// NodeControl is peer-only. It never accepts user credentials or grants a user
// read plan. Only identities already in the peer's exact allowlist may be warmed.
type WarmRequest struct {
	Version  string             `json:"version"`
	Identity RepositoryIdentity `json:"identity"`
}
type WarmResponse struct {
	Version    string             `json:"version"`
	Snapshot   RepositorySnapshot `json:"snapshot"`
	ObservedAt time.Time          `json:"observed_at"`
}
type NodeStatus struct {
	Version     string `json:"version"`
	EdgeID      string `json:"edge_id"`
	AuthorityID string `json:"authority_id"`
	Stores      int    `json:"stores"`
}
