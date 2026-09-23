package forgejointegration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func actionsLogCandidates(dir, owner, repo string, runID int64) ([]string, error) {
	root := strings.TrimSpace(dir)
	if root == "" || runID <= 0 {
		return nil, fmt.Errorf("actions log dir and run id are required")
	}
	owner, err := cleanActionsLogPart(owner)
	if err != nil {
		return nil, err
	}
	repo, err = cleanActionsLogPart(repo)
	if err != nil {
		return nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	shard := fmt.Sprintf("%02x", uint64(runID)&0xff)
	base := filepath.Join(absRoot, owner, repo, shard, fmt.Sprint(runID))
	candidates := []string{base + ".log.zst", base + ".log"}
	for _, candidate := range candidates {
		rel, err := filepath.Rel(absRoot, candidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("actions log path escapes configured directory")
		}
	}
	return candidates, nil
}

func readLocalActionsLog(dir, owner, repo string, runID int64) ([]byte, bool, error) {
	if strings.TrimSpace(dir) == "" || runID <= 0 {
		return nil, false, nil
	}
	candidates, err := actionsLogCandidates(dir, owner, repo, runID)
	if err != nil {
		return nil, false, err
	}
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, true, err
		}
		if strings.HasSuffix(candidate, ".zst") {
			text, err := decompressZstd(candidate)
			if err != nil {
				return nil, true, err
			}
			return text, true, nil
		}
		return data, true, nil
	}
	return nil, false, nil
}

func decompressZstd(path string) ([]byte, error) {
	out, err := exec.Command("zstd", "-dc", "--", path).Output()
	if err != nil {
		return nil, fmt.Errorf("decode forgejo actions log %s: %w", path, err)
	}
	return out, nil
}

func cleanActionsLogPart(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return "", fmt.Errorf("invalid actions log path part %q", value)
	}
	return value, nil
}
