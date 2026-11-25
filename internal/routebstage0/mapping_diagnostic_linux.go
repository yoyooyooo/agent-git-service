//go:build linux

package routebstage0

import (
	"errors"
	"fmt"
	"os"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

type mappingExpected struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type mappingDetail struct {
	Branch       string           `json:"branch"`
	LineNumber   int              `json:"line_number"`
	Line         string           `json:"line"`
	FieldCount   int              `json:"field_count"`
	CatalogMatch string           `json:"catalog_match"`
	Expected     *mappingExpected `json:"expected"`
}

// The error string remains the peer protocol's original refusal code. The
// single kernel row stays in root-private state, never in an exception or log.
// It is evidence of a refusal, not a new catalog member or execution authority.
type mappingRefusal struct {
	code       string
	mapsSHA256 string
	mapsBytes  int
	detail     mappingDetail
}

func (e *mappingRefusal) Error() string { return e.code }

func refusedMapping(code, branch string, raw []byte, number int, line string, fields int, match string, entry *interpreterauthority.FileIdentity) error {
	detail := mappingDetail{Branch: branch, LineNumber: number, Line: line, FieldCount: fields, CatalogMatch: match}
	if entry != nil {
		detail.Expected = &mappingExpected{Device: entry.Device, Inode: entry.Inode}
	}
	return &mappingRefusal{code: code, mapsSHA256: hash(raw), mapsBytes: len(raw), detail: detail}
}

type mappingFailure struct {
	Schema              string          `json:"schema"`
	RegistrationSHA256  string          `json:"registration_sha256"`
	CatalogSHA256       string          `json:"catalog_sha256"`
	InvocationID        string          `json:"invocation_id"`
	SubjectInvocationID string          `json:"subject_invocation_id"`
	Phase               string          `json:"phase"`
	BootID              string          `json:"boot_id"`
	Supervisor          ProcessIdentity `json:"supervisor"`
	Child               ProcessIdentity `json:"child"`
	Code                string          `json:"code"`
	MapsSHA256          string          `json:"maps_sha256"`
	MapsBytes           int             `json:"maps_bytes"`
	Mapping             mappingDetail   `json:"mapping"`
}

func (s *supervisor) mappingFailure(err error, subject Context) *mappingFailure {
	var failure *mappingRefusal
	if !errors.As(err, &failure) {
		return nil
	}
	return &mappingFailure{
		Schema: "ags.routeb-stage0-mapping-refusal.v1", RegistrationSHA256: s.loaded.registrationSHA,
		CatalogSHA256: s.loaded.registration.Interpreter.SHA256, InvocationID: s.loaded.invocation.ID,
		SubjectInvocationID: subject.InvocationID, Phase: subject.Phase, BootID: subject.BootID,
		Supervisor: subject.Supervisor, Child: subject.Child, Code: failure.code,
		MapsSHA256: failure.mapsSHA256, MapsBytes: failure.mapsBytes, Mapping: failure.detail,
	}
}

// Both callers have just checked this subject's process birth and parent. A
// runtime subject can differ from the requesting controller; retain both IDs.
// Failure to save diagnostics must not replace or weaken the original refusal.
func (s *supervisor) keepMappingFailure(err error, subject Context) {
	record := s.mappingFailure(err, subject)
	if record == nil || s.mappingFailureRecorded {
		return
	}
	s.mappingFailureRecorded = true
	if writeExclusive(s.loaded.statePath("failures", s.loaded.invocation.ID), record) != nil {
		fmt.Fprintln(os.Stderr, "code=stage0_mapping_diagnostic_unavailable")
	}
}
