package providerlogbridge

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func timestamp(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	t := time.Unix(value, 0).UTC()
	return &t
}

func (h *Handler) serveSteps(w http.ResponseWriter, r *http.Request, owner, repo string, task int64, binding logBinding) {
	var rows []struct {
		Name             string
		Index            int
		Status           int
		Started, Stopped int64
	}
	err := h.db.WithContext(r.Context()).Raw(`SELECT name, "index", status, started, stopped FROM action_task_step WHERE task_id = ? AND repo_id = (SELECT id FROM repository WHERE lower(owner_name)=lower(?) AND lower(lower_name)=lower(?)) ORDER BY "index" LIMIT 1001`, task, owner, repo).Scan(&rows).Error
	if err != nil || len(rows) > 1000 {
		h.fail(w, 502, "metadata_unavailable", "Provider step metadata unavailable")
		return
	}
	steps := make([]map[string]any, 0, len(rows))
	previous := -1
	for _, row := range rows {
		if row.Index < 0 || row.Index <= previous {
			h.fail(w, 502, "metadata_invalid", "Invalid provider steps")
			return
		}
		previous = row.Index
		status := ""
		var conclusion any
		switch row.Status {
		case 1, 2, 3, 4:
			status = "completed"
			conclusion = map[int]string{1: "success", 2: "failure", 3: "cancelled", 4: "skipped"}[row.Status]
		case 5, 7:
			status = "queued"
		case 6:
			status = "in_progress"
		case 0: // Unknown is not a successful or queued step.
		default:
			h.fail(w, 502, "metadata_invalid", "Invalid provider step status")
			return
		}
		steps = append(steps, map[string]any{"name": row.Name, "number": row.Index + 1, "status": status, "conclusion": conclusion, "started_at": timestamp(row.Started), "completed_at": timestamp(row.Stopped)})
	}
	verified, _ := verifyBinding(binding, mustExpected(r))
	h.write(w, 200, map[string]any{"schema": "ags.internal-provider-steps.v1", "repo": owner + "/" + repo, "task_id": task, "run_number": binding.RunNumber, "job_name": binding.JobName, "head_sha": binding.CommitSHA, "provider_pr": verified.ProviderPR, "provider_ref": verified.ProviderRef, "event": verified.Event, "steps": steps})
}
func mustExpected(r *http.Request) expectedBinding {
	expected, _ := parseExpectedBinding(r)
	return expected
}

type artifactRow struct {
	ID                        int64
	RunID                     int64
	Name                      string
	Size                      int64
	Status                    int
	Created, Updated, Expires int64
}

// ServeArtifacts exposes only repository/run-bound metadata and downloads. It
// never returns storage paths or redirects a caller to the provider.
func (h *Handler) ServeArtifacts(w http.ResponseWriter, r *http.Request) {
	peer, ok := r.Context().Value(socketPeerContextKey{}).(string)
	if r.Method != "GET" || !ok || !h.sourceAllowed(peer) || !h.authorized(r.Header.Get("Authorization")) {
		h.fail(w, 401, "unauthorized", "Provider bridge authentication failed")
		return
	}
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	run, err := strconv.ParseInt(chi.URLParam(r, "run"), 10, 64)
	if err != nil || run <= 0 || !safePart(owner) || !safePart(repo) || !canonicalSHA(r.URL.Query().Get("head_sha")) {
		h.fail(w, 404, "not_found", "Provider run not found")
		return
	}
	var count int64
	err = h.db.WithContext(r.Context()).Raw(`SELECT count(*) FROM action_run ar JOIN repository r ON r.id=ar.repo_id WHERE ar.id=? AND ar.commit_sha=? AND lower(r.owner_name)=lower(?) AND lower(r.lower_name)=lower(?)`, run, r.URL.Query().Get("head_sha"), owner, repo).Scan(&count).Error
	if err != nil {
		h.fail(w, 502, "database_unavailable", "Provider metadata unavailable")
		return
	}
	if count != 1 {
		h.fail(w, 404, "not_found", "Provider run not found")
		return
	}
	var rows []artifactRow
	err = h.db.WithContext(r.Context()).Raw(`SELECT min(a.id) AS id, a.run_id, a.artifact_name AS name, sum(a.file_size) AS size, max(a.status) AS status, min(a.created_unix) AS created, max(a.updated_unix) AS updated, min(a.expired_unix) AS expires FROM action_artifact a JOIN action_run ar ON ar.id=a.run_id AND ar.repo_id=a.repo_id WHERE a.run_id=? AND (a.status=2 OR a.status=4) GROUP BY a.run_id,a.artifact_name ORDER BY min(a.id) LIMIT 1001`, run).Scan(&rows).Error
	if err != nil || len(rows) > 1000 {
		h.fail(w, 502, "metadata_unavailable", "Provider artifact metadata unavailable")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		if a.ID <= 0 || a.Size < 0 || a.Name == "" {
			h.fail(w, 502, "metadata_invalid", "Invalid provider artifact")
			return
		}
		if download := r.URL.Query().Get("download"); download != "" {
			if download == strconv.FormatInt(a.ID, 10) {
				h.downloadArtifact(w, r, a)
				return
			}
			continue
		}
		items = append(items, map[string]any{"key": strconv.FormatInt(a.ID, 10), "run": strconv.FormatInt(run, 10), "name": a.Name, "size_in_bytes": a.Size, "expired": a.Status == 4, "created_at": timestamp(a.Created), "updated_at": timestamp(a.Updated), "expires_at": timestamp(a.Expires)})
	}
	if r.URL.Query().Get("download") != "" {
		h.fail(w, 404, "not_found", "Provider artifact not found")
		return
	}
	h.write(w, 200, map[string]any{"schema": "ags.internal-provider-artifacts.v1", "repo": owner + "/" + repo, "run": strconv.FormatInt(run, 10), "head_sha": r.URL.Query().Get("head_sha"), "artifacts": items})
}

func (h *Handler) downloadArtifact(w http.ResponseWriter, r *http.Request, a artifactRow) {
	if a.Status != 2 {
		h.fail(w, 410, "expired", "Provider artifact expired")
		return
	}
	if h.artifactRoot == "" {
		h.fail(w, 501, "storage_not_configured", "Provider artifact storage root not configured")
		return
	}
	var files []struct {
		StoragePath, ArtifactPath, ContentEncoding string
		Status                                     int
	}
	err := h.db.WithContext(r.Context()).Raw(`SELECT storage_path,artifact_path,content_encoding,status FROM action_artifact WHERE run_id=? AND artifact_name=? ORDER BY id LIMIT 1001`, a.RunID, a.Name).Scan(&files).Error
	if err != nil || len(files) == 0 || len(files) > 1000 {
		h.fail(w, 502, "storage_unavailable", "Provider artifact unavailable")
		return
	}
	root, err := os.OpenRoot(h.artifactRoot)
	if err != nil {
		h.fail(w, 502, "storage_unavailable", "Provider artifact unavailable")
		return
	}
	defer root.Close()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	total := int64(0)
	for _, f := range files {
		if f.Status != 2 || filepath.IsAbs(f.StoragePath) || strings.Contains(f.ArtifactPath, "\\") || filepath.IsAbs(f.ArtifactPath) || filepath.Clean(f.ArtifactPath) != f.ArtifactPath || f.ArtifactPath == ".." || strings.HasPrefix(f.ArtifactPath, "../") {
			h.fail(w, 502, "storage_invalid", "Invalid provider artifact path")
			return
		}
		file, e := root.Open(f.StoragePath)
		if e != nil {
			h.fail(w, 502, "storage_unavailable", "Provider artifact unavailable")
			return
		}
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() {
			file.Close()
			h.fail(w, 502, "storage_invalid", "Invalid provider artifact file")
			return
		}
		var reader io.Reader = file
		var gz *gzip.Reader
		if f.ContentEncoding == "gzip" {
			gz, e = gzip.NewReader(file)
			if e != nil {
				file.Close()
				h.fail(w, 502, "storage_invalid", "Invalid provider artifact encoding")
				return
			}
			reader = gz
		} else if f.ContentEncoding != "" && f.ContentEncoding != "application/zip" {
			file.Close()
			h.fail(w, 502, "storage_invalid", "Unsupported provider artifact encoding")
			return
		}
		body, e := io.ReadAll(io.LimitReader(reader, h.maxBytes-total+1))
		if gz != nil {
			gz.Close()
		}
		file.Close()
		total += int64(len(body))
		if e != nil || total > h.maxBytes {
			h.fail(w, 502, "storage_limit", "Provider artifact exceeds bounded download")
			return
		}
		if len(files) == 1 && f.ContentEncoding == "application/zip" && f.ArtifactPath == a.Name+".zip" {
			out.Write(body)
			break
		}
		entry, e := writer.Create(f.ArtifactPath)
		if e == nil {
			_, e = entry.Write(body)
		}
		if e != nil {
			h.fail(w, 502, "storage_invalid", "Provider artifact archive failed")
			return
		}
	}
	// A v4 artifact is already a zip; do not append another archive trailer.
	if !(len(files) == 1 && files[0].ContentEncoding == "application/zip" && files[0].ArtifactPath == a.Name+".zip") {
		if err = writer.Close(); err != nil {
			h.fail(w, 502, "storage_invalid", "Provider artifact archive failed")
			return
		}
	}
	if int64(out.Len()) > h.maxBytes {
		h.fail(w, 502, "storage_limit", "Provider artifact exceeds bounded download")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "artifact.zip"))
	w.Write(out.Bytes())
}
