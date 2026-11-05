package providerlogbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"unicode/utf8"

	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
)

type stepRow struct {
	RepoID, TaskRepoID  int64
	Name                string
	Index               int64
	Status              int
	Started, Stopped    int64
	LogIndex, LogLength int64
}

func (h *Handler) steps(ctx context.Context, task int64) ([]stepRow, error) {
	var rows []stepRow
	// Enumerate first, then validate every repository binding. A foreign row
	// must not turn a partial step list into an apparently complete one.
	err := h.db.WithContext(ctx).Raw(`SELECT s.repo_id, t.repo_id AS task_repo_id, s.name, s."index", s.status, s.started, s.stopped, s.log_index, s.log_length FROM action_task_step s JOIN action_task t ON t.id=s.task_id WHERE s.task_id=? ORDER BY s."index" LIMIT 1001`, task).Scan(&rows).Error
	if err != nil || len(rows) > 1000 {
		return nil, errors.New("provider step metadata unavailable")
	}
	previous := int64(-1)
	for _, row := range rows {
		if row.RepoID <= 0 || row.RepoID != row.TaskRepoID || row.Index < 0 || row.Index <= previous || row.Index >= math.MaxInt32-1 || row.Status < 0 || row.Status > 7 || len(row.Name) > 1024 {
			return nil, errors.New("invalid provider step metadata")
		}
		previous = row.Index
	}
	return rows, nil
}

func stepMetadata(row stepRow) providerlogprotocol.StepLog {
	step := providerlogprotocol.StepLog{Name: row.Name, Number: int(row.Index + 1), StartedAt: timestamp(row.Started), CompletedAt: timestamp(row.Stopped)}
	switch row.Status {
	case 1, 2, 3, 4:
		step.Status = "completed"
		step.Conclusion = map[int]string{1: "success", 2: "failure", 3: "cancelled", 4: "skipped"}[row.Status]
	case 5, 7:
		step.Status = "queued"
	case 6:
		step.Status = "in_progress"
	}
	return step
}

// stepLogLayout is optional. Incomplete, corrupt, expired or still changing
// metadata must leave the already verified whole-job log available.
func (h *Handler) stepLogLayout(ctx context.Context, task int64, rows []stepRow) *providerlogprotocol.StepLogs {
	if len(rows) == 0 {
		return nil
	}
	var tasks []struct {
		Status             int
		LogLength, LogSize int64
		LogIndexes         []byte
	}
	// Optional index work has its own bound. It never permits a larger log
	// read, and an unusually large index falls back to the unchanged job log.
	err := h.db.WithContext(ctx).Raw(`SELECT status, log_length, log_size, log_indexes FROM action_task WHERE id=? AND log_in_storage=true AND log_expired=false AND log_length BETWEEN 1 AND 1048576 AND log_size BETWEEN 1 AND ? AND length(log_indexes) <= ?`, task, h.maxBytes, h.maxBytes*2).Scan(&tasks).Error
	if err != nil || len(tasks) != 1 {
		return nil
	}
	t := tasks[0]
	if t.Status < 1 || t.Status > 4 {
		return nil
	}
	indexes, err := decodeLogIndexes(t.LogIndexes, t.LogLength, t.LogSize)
	if err != nil {
		return nil
	}
	layout := &providerlogprotocol.StepLogs{Schema: providerlogprotocol.StepLogsSchema, ByteLength: t.LogSize, Steps: make([]providerlogprotocol.StepLog, 0, len(rows)+1)}
	var gaps []providerlogprotocol.ByteRange
	previous := int64(0)
	failedStep := false
	for _, row := range rows {
		step := stepMetadata(row)
		if step.Status != "completed" || row.LogIndex < 0 || row.LogIndex > t.LogLength || row.LogLength < 0 || row.LogLength > t.LogLength-row.LogIndex {
			return nil
		}
		if row.LogLength > 0 {
			if row.LogIndex < previous {
				return nil
			}
			if row.LogIndex > previous {
				gaps = append(gaps, providerlogprotocol.ByteRange{Start: indexes[previous], End: indexes[row.LogIndex]})
			}
			end := row.LogIndex + row.LogLength
			step.Ranges = []providerlogprotocol.ByteRange{{Start: indexes[row.LogIndex], End: indexes[end]}}
			previous = end
			failedStep = failedStep || row.Status == 2
		}
		layout.Steps = append(layout.Steps, step)
	}
	// A failure outside every recorded failed step cannot be attributed with
	// confidence. Do not let gh's step mode hide the entire failure.
	if t.Status == 2 && !failedStep {
		return nil
	}
	if previous < t.LogLength {
		gaps = append(gaps, providerlogprotocol.ByteRange{Start: indexes[previous], End: t.LogSize})
	}
	if len(gaps) > 0 {
		if len(layout.Steps) >= 1000 {
			return nil
		}
		layout.Steps = append(layout.Steps, providerlogprotocol.StepLog{
			Name: providerlogprotocol.UnattributedStepName, Number: layout.Steps[len(layout.Steps)-1].Number + 1,
			Status: "completed", Conclusion: map[int]string{1: "success", 2: "failure", 3: "cancelled", 4: "skipped"}[t.Status],
			Ranges: gaps, Diagnostic: "unattributed",
		})
	}
	encoded, err := json.Marshal(layout)
	if err != nil || len(encoded) > providerlogprotocol.StepLogMetadataMaxBytes {
		return nil
	}
	return layout
}

// Forgejo 12 models/actions/utils.go encodes absolute decoded byte offsets with
// binary.PutVarint (signed varints), not JSON, unsigned varints or byte deltas.
func decodeLogIndexes(data []byte, count, size int64) ([]int64, error) {
	if count <= 0 || count > 1<<20 || size <= 0 {
		return nil, errors.New("invalid provider log index bounds")
	}
	reader := bytes.NewReader(data)
	indexes := make([]int64, 0, count+1)
	for i := int64(0); i < count; i++ {
		offset, err := binary.ReadVarint(reader)
		if err != nil || offset < 0 || offset >= size || (i == 0 && offset != 0) || (i > 0 && offset <= indexes[len(indexes)-1]) {
			return nil, errors.New("invalid provider log indexes")
		}
		indexes = append(indexes, offset)
	}
	if reader.Len() != 0 {
		return nil, errors.New("provider log index count mismatch")
	}
	return append(indexes, size), nil
}

func (h *Handler) logSteps(ctx context.Context, task int64, text []byte) *providerlogprotocol.StepLogs {
	// JSON replaces invalid UTF-8. Those replacements would invalidate byte
	// coordinates, so retain whole-job mode for that historical provider log.
	if !utf8.Valid(text) {
		return nil
	}
	rows, err := h.steps(ctx, task)
	if err != nil {
		return nil
	}
	layout := h.stepLogLayout(ctx, task, rows)
	if layout == nil || layout.ByteLength != int64(len(text)) {
		return nil
	}
	for _, step := range layout.Steps {
		for _, span := range step.Ranges {
			if (span.Start > 0 && text[span.Start-1] != '\n') || (span.End < int64(len(text)) && text[span.End-1] != '\n') {
				return nil
			}
		}
	}
	return layout
}
