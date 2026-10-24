package cibackend

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func fixtureCurrentJobs(first, count int, prefix string, offset int) map[int64]string {
	jobs := map[int64]string{}
	for i := 0; i < count; i++ {
		jobs[int64(first-i)] = prefix + strconv.Itoa(offset+i)
	}
	return jobs
}

func withCurrentJobFixture(jobs map[int64]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/internal/provider-logs/") && strings.HasSuffix(r.URL.Path, "/runs/17/jobs") {
			items := []any{}
			for id, name := range jobs {
				items = append(items, map[string]any{"task_id": id, "name": name})
			}
			json.NewEncoder(w).Encode(map[string]any{"schema": "ags.internal-provider-jobs.v1", "repo": "ci/project", "run": "17", "run_number": func() int64 { n, _ := strconv.ParseInt(r.URL.Query().Get("run_number"), 10, 64); return n }(), "head_sha": strings.Repeat("a", 40), "jobs": items})
			return
		}
		next.ServeHTTP(w, r)
	})
}
