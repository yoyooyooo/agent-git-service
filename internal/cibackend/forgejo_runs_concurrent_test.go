package cibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type runPageTransport func(*http.Request) (*http.Response, error)

func (f runPageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type runPageFlights struct {
	mu      sync.Mutex
	active  int
	maximum int
	queries map[int]url.Values
	counts  map[int]int
}

func (f *runPageFlights) activeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func (f *runPageFlights) begin(r *http.Request) (int, func()) {
	query := r.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	f.mu.Lock()
	f.active++
	f.maximum = max(f.maximum, f.active)
	f.queries[page] = query
	f.counts[page]++
	f.mu.Unlock()
	return page, func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}
}

func runPageResponse(r *http.Request, status int, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
}

func concurrentRunBackend(t *testing.T, transport runPageTransport) *Forgejo {
	t.Helper()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: "https://ci.example.test", TokenFile: "fixture"}, "provider-only", "")
	require.NoError(t, err)
	f := backend.(*Forgejo)
	f.client.Transport = transport
	return f
}

type runReadResult struct {
	runs Runs
	err  error
}

func TestForgejoRunInventoryParallelPagesAreBoundedAndOrdered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A bubble plus channel barriers proves concurrency without timing or
		// network assumptions. Serial retrieval cannot reach all four barriers.
		const total = 1335
		items := forgejoRunInventoryFixture(total)
		for _, item := range items {
			item["prettyref"], item["event"] = "#42", "pull_request_sync"
		}
		gates := map[int]chan struct{}{2: make(chan struct{}), 3: make(chan struct{}), 4: make(chan struct{}), 5: make(chan struct{})}
		released := map[int]bool{}
		release := func(page int) {
			if !released[page] {
				close(gates[page])
				released[page] = true
			}
		}
		defer func() {
			for page := range gates {
				release(page)
			}
		}()
		flights := &runPageFlights{queries: map[int]url.Values{}, counts: map[int]int{}}
		backend := concurrentRunBackend(t, func(r *http.Request) (*http.Response, error) {
			page, finish := flights.begin(r)
			defer finish()
			if gate := gates[page]; gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			start := (page - 1) * 50
			if start < 0 || start >= total {
				return nil, fmt.Errorf("unexpected provider page %d", page)
			}
			return runPageResponse(r, 200, map[string]any{"total_count": total, "workflow_runs": items[start:min(start+50, total)]})
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan runReadResult, 1)
		go func() {
			runs, err := backend.Runs(ctx, "ci/project", Query{Page: 2, PerPage: 100, HeadSHA: strings.Repeat("a", 40), Event: "pull_request", Status: "completed"})
			done <- runReadResult{runs, err}
		}()
		synctest.Wait()
		require.Equal(t, 4, flights.activeCount(), "the remaining inventory must progress with four bounded requests")
		require.Equal(t, 4, flights.maximum)
		require.Equal(t, map[int]int{1: 1, 2: 1, 3: 1, 4: 1, 5: 1}, flights.counts)
		// Finish page 5 first. That worker must fetch the rest while 2–4 stay
		// blocked; completion order must never determine page row counts.
		release(5)
		synctest.Wait()
		require.Equal(t, 3, flights.activeCount())
		require.Equal(t, 4, flights.maximum)
		require.Len(t, flights.counts, 27)
		select {
		case result := <-done:
			t.Fatalf("returned an incomplete inventory before all pages finished: %+v", result)
		default:
		}
		for _, page := range []int{2, 3, 4} {
			release(page)
		}
		result := <-done
		require.NoError(t, result.err)
		require.Equal(t, total, result.runs.Total)
		require.Len(t, result.runs.Items, 100)
		require.False(t, result.runs.Complete)
		for i, run := range result.runs.Items {
			require.Equal(t, int64(total-100-i), run.Number)
		}
		synctest.Wait()
		require.Zero(t, flights.activeCount())
		for page := 1; page <= 27; page++ {
			require.Equal(t, 1, flights.counts[page])
			query := flights.queries[page]
			require.Equal(t, strconv.Itoa(page), query.Get("page"))
			require.Equal(t, "50", query.Get("limit"))
			require.Equal(t, strings.Repeat("a", 40), query.Get("head_sha"))
			require.Equal(t, []string{"pull_request", "pull_request_sync"}, query["event"])
			require.Equal(t, []string{"success", "failure", "cancelled", "skipped"}, query["status"])
		}
	})
}

func TestForgejoRunInventoryErrorsCancelAllOtherPages(t *testing.T) {
	for _, mode := range []string{"forbidden", "unavailable", "metadata", "changed-total", "short-page", "duplicate-id", "duplicate-number"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const total = 1335
				items := forgejoRunInventoryFixture(total)
				failPage := make(chan struct{})
				type cancellation struct {
					page int
					err  error
				}
				cancelled := make(chan cancellation, 4)
				flights := &runPageFlights{queries: map[int]url.Values{}, counts: map[int]int{}}
				backend := concurrentRunBackend(t, func(r *http.Request) (*http.Response, error) {
					page, finish := flights.begin(r)
					defer finish()
					if page == 2 {
						select {
						case <-failPage:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					} else if page > 1 {
						<-r.Context().Done()
						cancelled <- cancellation{page, r.Context().Err()}
						return nil, r.Context().Err()
					}
					start := (page - 1) * 50
					batch := append([]map[string]any(nil), items[start:min(start+50, total)]...)
					count, status := total, 200
					if page == 2 {
						batch[0] = maps.Clone(batch[0])
						switch mode {
						case "forbidden":
							status = 403
						case "unavailable":
							status = 500
						case "metadata":
							batch[0]["status"] = "invalid-provider-status"
						case "changed-total":
							count++
						case "short-page":
							batch = batch[:49]
						case "duplicate-id":
							batch[0]["id"] = items[0]["id"]
						case "duplicate-number":
							batch[0]["index_in_repo"] = items[0]["index_in_repo"]
						}
					}
					return runPageResponse(r, status, map[string]any{"total_count": count, "workflow_runs": batch})
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan runReadResult, 1)
				go func() {
					runs, err := backend.Runs(ctx, "ci/project", Query{PerPage: 100})
					done <- runReadResult{runs, err}
				}()
				synctest.Wait()
				require.Equal(t, 4, flights.activeCount())
				close(failPage)
				result := <-done
				synctest.Wait()
				want := ErrUnavailable
				if mode == "forbidden" {
					want = ErrForbidden
				} else if mode == "metadata" {
					want = ErrInvalid
				}
				require.ErrorIs(t, result.err, want, "the initiating error must survive cancellation of sibling requests")
				require.NoError(t, ctx.Err(), "worker failure must not cancel the caller's context")
				require.Empty(t, result.runs.Items)
				require.Zero(t, result.runs.Total)
				require.False(t, result.runs.Complete)
				require.Zero(t, flights.activeCount(), "all sibling reads must finish before return")
				require.Equal(t, map[int]int{1: 1, 2: 1, 3: 1, 4: 1, 5: 1}, flights.counts, "failure must not start queued pages")
				require.Len(t, cancelled, 3)
				seen := map[int]bool{}
				for range 3 {
					stop := <-cancelled
					require.ErrorIs(t, stop.err, context.Canceled, "cancellation must be immediate, not the HTTP timeout")
					seen[stop.page] = true
				}
				require.Equal(t, map[int]bool{3: true, 4: true, 5: true}, seen)
			})
		})
	}
}

func TestForgejoRunInventoryCancellationJoinsEveryPage(t *testing.T) {
	for _, phase := range []string{"before-first", "first-page", "remaining-pages", "deadline"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const total = 1335
				items := forgejoRunInventoryFixture(total)
				cancelled := make(chan error, 4)
				flights := &runPageFlights{queries: map[int]url.Values{}, counts: map[int]int{}}
				backend := concurrentRunBackend(t, func(r *http.Request) (*http.Response, error) {
					page, finish := flights.begin(r)
					defer finish()
					if page == 1 && (phase == "remaining-pages" || phase == "deadline") {
						return runPageResponse(r, 200, map[string]any{"total_count": total, "workflow_runs": items[:50]})
					}
					<-r.Context().Done()
					cancelled <- r.Context().Err()
					return nil, r.Context().Err()
				})
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if phase == "before-first" {
					cancel()
				}
				done := make(chan runReadResult, 1)
				go func() {
					runs, err := backend.Runs(ctx, "ci/project", Query{PerPage: 100})
					done <- runReadResult{runs, err}
				}()
				synctest.Wait()
				active := map[string]int{"before-first": 0, "first-page": 1, "remaining-pages": 4, "deadline": 4}[phase]
				require.Equal(t, active, flights.activeCount())
				want := context.Canceled
				if phase == "deadline" {
					// This advances only synctest's fake clock, not wall time.
					time.Sleep(5 * time.Second)
					want = context.DeadlineExceeded
				} else {
					cancel()
				}
				result := <-done
				synctest.Wait()
				require.ErrorIs(t, result.err, want, "the caller cancellation takes precedence over HTTP transport errors")
				require.Empty(t, result.runs.Items)
				require.Zero(t, result.runs.Total)
				require.False(t, result.runs.Complete)
				require.Zero(t, flights.activeCount())
				require.Len(t, cancelled, active)
				for range active {
					require.ErrorIs(t, <-cancelled, want)
				}
				count := active
				if active == 4 {
					count++ // The first page finished before the parallel reads.
				}
				require.Len(t, flights.counts, count, "cancellation must leave queued requests unstarted")
			})
		})
	}
}
