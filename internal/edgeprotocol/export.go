package edgeprotocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const (
	ExportPath         = "/_ags/edge/v1/export"
	ExportContentType  = "application/x-ags-git-snapshot-v1"
	MaxDescriptorBytes = 8192
)

// Export framing is a uint32 big-endian canonical-manifest length, manifest,
// then a complete non-thin native Git pack ending at HTTP EOF. The pack is
// verified by the consumer before publication. No caller-selected path/URL or
// original user credential is part of replication transport.
func WriteExportHeader(w io.Writer, m Manifest) error {
	data, err := EncodeManifest(m)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func ReadExportHeader(r io.Reader) (Manifest, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Manifest{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxManifestBytes {
		return Manifest{}, errors.New("invalid export manifest length")
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(r, data); err != nil {
		return Manifest{}, err
	}
	return DecodeManifest(bytes.NewReader(data))
}

func EncodeSnapshot(s RepositorySnapshot) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxDescriptorBytes {
		return nil, errors.New("snapshot descriptor too large")
	}
	return data, nil
}

func DecodeSnapshot(r io.Reader) (RepositorySnapshot, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxDescriptorBytes+1))
	if err != nil {
		return RepositorySnapshot{}, err
	}
	if len(data) > MaxDescriptorBytes {
		return RepositorySnapshot{}, errors.New("snapshot descriptor too large")
	}
	var s RepositorySnapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return RepositorySnapshot{}, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return RepositorySnapshot{}, errors.New("trailing descriptor data")
	}
	canonical, err := EncodeSnapshot(s)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(data), canonical) {
		return RepositorySnapshot{}, errors.New("noncanonical descriptor")
	}
	return s, nil
}
