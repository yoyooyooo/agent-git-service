package edgeprotocol

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var (
	ErrReadDenied      = errors.New("edge read denied")
	ErrReadUnavailable = errors.New("edge read unavailable")
)

const (
	PreparePath     = "/_ags/edge/v1/read/prepare"
	RevalidatePath  = "/_ags/edge/v1/read/revalidate"
	PrepareVersion  = "ags.edge.prepare.v1"
	MaxControlBytes = 32 << 10
)

// PrepareRead selects discovery from current Git, or an EXACT retained view
// for a subsequent fetch. Selecting a view is not user/peer authorization.
// The public path must resolve to Identity at the primary; a rename alias may
// resolve there, but a same-name replacement cannot inherit the old StoreID.
// Git packet parsing/OID-based selection is owned by the later Edge read path.
type PrepareRead struct {
	Version    string              `json:"version"`
	RequestID  string              `json:"request_id"`
	Repository string              `json:"repository"`
	Identity   RepositoryIdentity  `json:"identity"`
	Phase      string              `json:"phase"`
	Snapshot   *RepositorySnapshot `json:"snapshot,omitempty"`
}

func (p PrepareRead) Validate() error {
	if p.Version != PrepareVersion || !identifier(p.RequestID) || p.Identity.Validate() != nil {
		return errors.New("invalid read request binding")
	}
	parts := strings.Split(p.Repository, "/")
	if len(parts) != 2 || len(p.Repository) > 512 {
		return errors.New("invalid repository locator")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return errors.New("invalid repository locator")
		}
	}
	switch p.Phase {
	case "discover":
		if p.Snapshot != nil {
			return errors.New("discovery cannot select an old snapshot")
		}
	case "fetch":
		if p.Snapshot == nil || p.Snapshot.Validate() != nil || p.Snapshot.Identity != p.Identity {
			return errors.New("fetch requires an exact bound snapshot")
		}
	default:
		return errors.New("unsupported read phase")
	}
	return nil
}

// DecodeControl enforces a bounded, closed JSON envelope and exactly one
// value. Callers must ALSO validate the typed binding and authenticate both
// principals. This function accepts no transport credentials in JSON.
func DecodeControl(r io.Reader, dst any) error {
	data, err := io.ReadAll(io.LimitReader(r, MaxControlBytes+1))
	if err != nil || len(data) > MaxControlBytes {
		return errors.New("invalid control body size")
	}
	if err := uniqueControlFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.New("invalid control envelope")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing control value")
	}
	return nil
}
