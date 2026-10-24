package forgejointegration

// RefOperationContractVersion names a design/wire contract, not an installed
// API or receiver. No executor, route, configuration flag or migration exists
// in S0/S1. Capability reports cannot be used as an enablement mechanism.
const RefOperationContractVersion = "ref_operations.v1"

type RefOperationCapabilities struct {
	Execute          bool   `json:"execute"`
	Retire           bool   `json:"retire"`
	StrictFence      bool   `json:"strict_fence"`
	ReversePRHistory bool   `json:"reverse_pr_history"`
	Reason           string `json:"reason"`
}

func RefCapabilities() RefOperationCapabilities {
	return RefOperationCapabilities{Reason: "strict_fence_unavailable"}
}

// Immutable identities must not be derived from display paths or mapping names.
type RefRepositoryIdentity struct {
	InstanceID   string `json:"instance_id"`
	RepositoryID string `json:"repository_id"`
}
type RefRevision struct {
	Ref      string  `json:"ref"`
	OID      *string `json:"oid"`      // nil only for successfully observed absence
	Revision uint64  `json:"revision"` // delete leaves a tombstone; never reset
}
type RefRevisionVector struct {
	SourceRepo        uint64        `json:"source_repo"`
	ProviderRepo      uint64        `json:"provider_repo"`
	SourceRefs        []RefRevision `json:"source_refs"`
	ProviderRefs      []RefRevision `json:"provider_refs"`
	ReversePRDomain   string        `json:"reverse_pr_domain"`
	ReversePRRevision uint64        `json:"reverse_pr_revision"`
	HistoryWatermark  string        `json:"history_watermark"`
	Mapping           uint64        `json:"mapping"`
	Policy            uint64        `json:"policy"`
	Credential        uint64        `json:"credential"` // non-secret generation, never token hash
	Authority         uint64        `json:"authority"`
	Revocation        uint64        `json:"revocation"`
}

// B is signed and immutable. C is only a verified committed journal prefix of B;
// observing current heads/revisions is never sufficient to advance C.
type RefBaseline struct {
	Source          RefRepositoryIdentity `json:"source"`
	Provider        RefRepositoryIdentity `json:"provider"`
	Revisions       RefRevisionVector     `json:"revisions"`
	InventoryDigest string                `json:"inventory_digest"`
	SelectionDigest string                `json:"selection_digest"`
}
type RefExecutionCursor struct {
	CursorVersion      uint64            `json:"cursor_version"`
	Revisions          RefRevisionVector `json:"revisions"`
	LastReceiptID      string            `json:"last_receipt_id"`
	ReceiptChainDigest string            `json:"receipt_chain_digest"`
	NextItem           uint64            `json:"next_item"`
}

// A receiver must atomically commit refs AND durable journal, or provide an
// independently proven unambiguous recovery protocol. This is only a schema.
type RefEffectReceipt struct {
	ReceiverIdentity      string                `json:"receiver_identity"`
	Source                RefRepositoryIdentity `json:"source"`
	Provider              RefRepositoryIdentity `json:"provider"`
	OperationID           string                `json:"operation_id"`
	ItemID                string                `json:"item_id"`
	EffectID              string                `json:"effect_id"`
	Epoch                 uint64                `json:"epoch"`
	PlanDigest            string                `json:"plan_digest"`
	InputCursorDigest     string                `json:"input_cursor_digest"`
	ReceiptID             string                `json:"receipt_id"`
	PreviousReceiptID     string                `json:"previous_receipt_id"`
	Sequence              uint64                `json:"sequence"`
	CommandKind           string                `json:"command_kind"` // ff or archive_create_and_retire only
	Before                RefRevisionVector     `json:"before"`
	After                 RefRevisionVector     `json:"after"`
	PolicyDigest          string                `json:"policy_digest"`
	DerivedPRFieldsDigest string                `json:"derived_pr_fields_digest"`
	Committed             bool                  `json:"committed"`
	Authentication        string                `json:"authentication"` // receiver signature, not a credential
}

// Reverse membership is keyed by immutable head repository as well as base.
// Current pulls on one base repo are NOT a complete reverse/history inventory.
type RefPRIdentity struct {
	Repository RefRepositoryIdentity `json:"repository"`
	ID         string                `json:"id"`
}
type RefHistoricalRoot struct {
	PR                *RefPRIdentity        `json:"pr"`
	Origin            RefRepositoryIdentity `json:"origin"`
	LogicalRef        string                `json:"logical_ref"`
	Kind              string                `json:"kind"` // head/base/merge-base/merge/squash/result/pr-ref/backup/comment
	OID               string                `json:"oid"`
	ObjectType        string                `json:"object_type"`
	HistoryEvidenceID string                `json:"history_evidence_id"`
}
type RefHistoryCoverage struct {
	Domain         string              `json:"domain"`
	Revision       uint64              `json:"revision"`
	Watermark      string              `json:"watermark"`
	Complete       bool                `json:"complete"` // unknown/missing pages => false, never infer true
	Roots          []RefHistoricalRoot `json:"roots"`
	RootsDigest    string              `json:"roots_digest"`
	PRRefMapDigest string              `json:"pr_ref_map_digest"`
}

// ReviewSubject excludes plan id/TTL/review ids to avoid approval recursion.
// Immutable subject remains bound to B even after a legitimate C prefix; a
// semantic change of unique/PR/R dependencies requires split/replan/review.
type RefRetirementReviewSubject struct {
	SchemaVersion               string                `json:"schema_version"`
	PlannerVersion              string                `json:"planner_version"`
	SourceCodeVersion           string                `json:"source_code_version"`
	Source                      RefRepositoryIdentity `json:"source"`
	Provider                    RefRepositoryIdentity `json:"provider"`
	Mode                        string                `json:"mode"` // retire only
	Ref                         string                `json:"ref"`
	F                           string                `json:"f"`
	ObjectFormat                string                `json:"object_format"`
	Baseline                    RefRevisionVector     `json:"baseline"`
	InventoryDigest             string                `json:"inventory_digest"`
	AuthoritativeBaselineDigest string                `json:"authoritative_baseline_digest"`
	OwnershipSourceKind         string                `json:"ownership_source_kind"`
	OwnershipEvidenceDigest     string                `json:"ownership_evidence_digest"`
	UniqueSetDigest             string                `json:"unique_set_digest"`
	UniqueCount                 uint64                `json:"unique_count"`
	PRHistoryImpactDigest       string                `json:"pr_history_impact_digest"`
	Recovery                    RefHistoryCoverage    `json:"recovery"`
	ArchiveACLRetentionDigest   string                `json:"archive_acl_retention_digest"`
	AllowedEffectSpecDigest     string                `json:"allowed_effect_spec_digest"`
}

type RefReviewDecisions struct {
	AttestOwnership                bool `json:"attest_ownership"`
	ConfirmClosedMergedPRImpact    bool `json:"confirm_closed_merged_pr_impact"`
	ApproveUniqueSet               bool `json:"approve_unique_set"`
	AcceptArchiveRecoveryRetention bool `json:"accept_archive_recovery_and_retention"`
}

// Reverse membership records are distinct from historical roots and coverage.
// An empty current-membership list cannot prove no historical PR ever existed.
type RefPRMembership struct {
	PR               RefPRIdentity         `json:"pr"`
	HeadRepository   RefRepositoryIdentity `json:"head_repository"`
	HeadRef          string                `json:"head_ref"`
	BaseRepository   RefRepositoryIdentity `json:"base_repository"`
	BaseRef          string                `json:"base_ref"`
	State            string                `json:"state"`
	MutationSequence uint64                `json:"mutation_sequence"`
	HistoryDigest    string                `json:"history_digest"`
}

// This is server-loaded trusted record metadata, not a client reviewer claim.
// TTL/revocation and current authority must still be checked at actual commit.
type RefReviewBinding struct {
	ReviewID           string             `json:"review_id"`
	Version            uint64             `json:"version"`
	RecordDigest       string             `json:"record_digest"`
	SubjectDigest      string             `json:"subject_digest"`
	ReviewerSubjectID  string             `json:"reviewer_subject_id"`
	ReviewerIssuer     string             `json:"reviewer_issuer"`
	AuthorityRevision  uint64             `json:"authority_revision"`
	RevocationRevision uint64             `json:"revocation_revision"`
	ExpiresAt          string             `json:"expires_at"`
	Decisions          RefReviewDecisions `json:"decisions"`
}
