package cibackend

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
)

type forgejoJobObservation struct {
	Schema      string `json:"schema"`
	Repo        string `json:"repo"`
	TaskID      int64  `json:"task_id"`
	RunNumber   int64  `json:"run_number"`
	JobName     string `json:"job_name"`
	Head        string `json:"head_sha"`
	ProviderPR  int    `json:"provider_pr"`
	ProviderRef string `json:"provider_ref"`
	Event       string `json:"event"`
	Text        string `json:"text"`
	Steps       []Step `json:"steps"`
	// Optional metadata must not make a valid whole-job log unreadable.
	StepLogs json.RawMessage `json:"step_logs"`
}

type forgejoLogEntry struct {
	name string
	body []byte
}

// Match official gh's ZIP lookup, including a cut through a UTF-16 surrogate
// pair. A different sanitizer would make gh miss the files or select a collision.
func ghLogJobName(name string) string {
	name = strings.NewReplacer("/", "", ":", "").Replace(name)
	units := utf16.Encode([]rune(name))
	if len(units) > 90 {
		name = string(utf16.Decode(units[:90]))
	}
	return strings.TrimSpace(name)
}

func safeGHLogJobName(name string) bool {
	for _, char := range name {
		if char == '\\' || char < 32 || char == 127 {
			return false
		}
	}
	return true
}

// A usable layout covers every decoded byte exactly once. The diagnostic
// bucket is explicit and keeps setup/post output visible without attributing it
// to an unrelated provider step. Invalid or legacy layouts return no entries so
// the caller can retain the complete, already bound job log.
func forgejoStepLogEntries(job Job, name string, text []byte, raw json.RawMessage) []forgejoLogEntry {
	if job.Status != "completed" || !utf8.Valid(text) || len(raw) > providerlogprotocol.StepLogMetadataMaxBytes {
		return nil
	}
	var layout providerlogprotocol.StepLogs
	if json.Unmarshal(raw, &layout) != nil || layout.Schema != providerlogprotocol.StepLogsSchema || layout.ByteLength != int64(len(text)) || len(layout.Steps) == 0 || len(layout.Steps) > 1000 {
		return nil
	}
	var coverage []providerlogprotocol.ByteRange
	previous := 0
	failedStep := false
	for i, step := range layout.Steps {
		if step.Number <= previous || step.Status != "completed" {
			return nil
		}
		if _, _, err := normalized(step.Status, step.Conclusion); err != nil {
			return nil
		}
		if step.Diagnostic != "" {
			if step.Diagnostic != "unattributed" || step.Name != providerlogprotocol.UnattributedStepName || i != len(layout.Steps)-1 || i == 0 || step.Number != previous+1 || step.Conclusion != job.Conclusion || len(step.Ranges) == 0 || step.StartedAt != nil || step.CompletedAt != nil {
				return nil
			}
		} else {
			if len(step.Ranges) > 1 {
				return nil
			}
			failedStep = failedStep || (step.Conclusion == "failure" && len(step.Ranges) > 0)
		}
		previous = step.Number
		var previousEnd int64 = -1
		for _, span := range step.Ranges {
			if span.Start < 0 || span.End <= span.Start || span.End > int64(len(text)) || span.Start < previousEnd || (span.Start > 0 && text[span.Start-1] != '\n') || (span.End < int64(len(text)) && text[span.End-1] != '\n') {
				return nil
			}
			previousEnd = span.End
			coverage = append(coverage, span)
		}
	}
	if job.Conclusion == "failure" && !failedStep {
		return nil
	}
	sort.Slice(coverage, func(i, j int) bool { return coverage[i].Start < coverage[j].Start })
	end := int64(0)
	for _, span := range coverage {
		if span.Start != end {
			return nil
		}
		end = span.End
	}
	if end != int64(len(text)) {
		return nil
	}
	entries := make([]forgejoLogEntry, 0, len(layout.Steps))
	for _, step := range layout.Steps {
		var body []byte
		for _, span := range step.Ranges {
			body = append(body, text[span.Start:span.End]...)
		}
		entries = append(entries, forgejoLogEntry{name: name + "/" + strconv.Itoa(step.Number) + "_step.txt", body: body})
	}
	return entries
}
