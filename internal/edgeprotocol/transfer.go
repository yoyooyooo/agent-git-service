package edgeprotocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const (
	TransferPath           = "/_ags/edge/v2/export"
	TransferVersion        = "ags.edge.transfer.v2"
	TransferContentType    = "application/x-ags-git-snapshot-v2"
	maxTransferHeaderBytes = MaxManifestBytes + 2*MaxDescriptorBytes
)

// TransferRequest nominates ONE exact verified base, not arbitrary have OIDs.
// Both ends pin it for the transfer. Peer and original-user authorization are
// unchanged. A missing retained base permits an explicitly declared full pack
// of the SAME target; denial/corruption must never silently downgrade.
type TransferRequest struct {
	Version string              `json:"version"`
	Target  RepositorySnapshot  `json:"target"`
	Base    *RepositorySnapshot `json:"base,omitempty"`
}

// TransferHeader describes either a complete pack (Base=nil), or exactly
// objects(target) minus objects(base). Delta packs are non-thin: compression
// bases are inside the pack, although Git graph parents may be in Base.
type TransferHeader struct {
	Version  string              `json:"version"`
	Manifest Manifest            `json:"manifest"`
	Base     *RepositorySnapshot `json:"base,omitempty"`
}

func CompatibleBase(target, base RepositorySnapshot) bool {
	return target.Validate() == nil && base.Validate() == nil && target.Identity == base.Identity &&
		target.ObjectFormat == base.ObjectFormat && target.ExportPolicyRevision == base.ExportPolicyRevision
}

func (r TransferRequest) Validate() error {
	if r.Version != TransferVersion || r.Target.Validate() != nil || r.Base != nil && !CompatibleBase(r.Target, *r.Base) {
		return errors.New("invalid transfer binding")
	}
	return nil
}
func (h TransferHeader) Validate() error {
	if h.Version != TransferVersion || h.Manifest.Validate() != nil || h.Base != nil && !CompatibleBase(h.Manifest.Snapshot, *h.Base) {
		return errors.New("invalid transfer header")
	}
	return nil
}
func EncodeTransferRequest(r TransferRequest) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}
func DecodeTransferRequest(r io.Reader) (TransferRequest, error) {
	data, err := io.ReadAll(io.LimitReader(r, 2*MaxDescriptorBytes+1025))
	if err != nil || len(data) > 2*MaxDescriptorBytes+1024 {
		return TransferRequest{}, errors.New("invalid transfer request size")
	}
	var request TransferRequest
	if err := decodeCanonical(data, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}
func WriteTransferHeader(w io.Writer, h TransferHeader) error {
	if err := h.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil || len(data) > maxTransferHeaderBytes {
		return errors.New("invalid transfer header size")
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
func ReadTransferHeader(r io.Reader) (TransferHeader, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return TransferHeader{}, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n == 0 || n > maxTransferHeaderBytes {
		return TransferHeader{}, errors.New("invalid transfer header size")
	}
	data := make([]byte, int(n))
	if _, err := io.ReadFull(r, data); err != nil {
		return TransferHeader{}, err
	}
	var h TransferHeader
	if err := decodeCanonical(data, &h); err != nil {
		return h, err
	}
	return h, h.Validate()
}
func decodeCanonical(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(dst) != nil || dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid closed transfer JSON")
	}
	canonical, err := json.Marshal(dst)
	if err != nil || !bytes.Equal(bytes.TrimSpace(data), canonical) {
		return errors.New("noncanonical transfer JSON")
	}
	return nil
}
