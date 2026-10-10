package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestDisabledSquashMergeIs409OnRESTAndGraphQL(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repo, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: h.User.Login, Name: "merge-gate", AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Svc.Git.CreateBranch(ctx, repo.FullName, "feature", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Svc.Git.WriteFile(ctx, repo.FullName, "feature", "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatal(err)
	}
	auth := service.ContextWithUser(ctx, h.User)
	pr, err := h.Svc.CreatePR(auth, service.CreatePRInput{
		RepoFullName: repo.FullName, Title: "gate", HeadRef: "feature", BaseRef: "main", AuthorLogin: h.User.Login,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.DB.Model(&db.Repository{}).Where("id = ?", repo.ID).Update("allow_squash_merge", false).Error; err != nil {
		t.Fatal(err)
	}
	before, err := h.Svc.Git.HeadSHA(ctx, repo.FullName, "main")
	if err != nil {
		t.Fatal(err)
	}

	rest := h.DoRESTJSON(t, http.MethodPut, "/api/v3/repos/"+repo.FullName+"/pulls/1/merge", map[string]any{
		"merge_method": "squash", "sha": pr.HeadSHA,
	})
	assertStatusCode(t, rest, http.StatusConflict)
	if !strings.Contains(rest.Body.String(), "squash") {
		t.Fatalf("REST body=%s", rest.Body.String())
	}

	payload, _ := json.Marshal(map[string]any{
		"query": `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){pullRequest{state}}}`,
		"variables": map[string]any{"input": map[string]any{
			"pullRequestId": "PullRequest_" + strconv.FormatUint(uint64(pr.ID), 10), "mergeMethod": "SQUASH", "expectedHeadOid": pr.HeadSHA,
		}},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(payload))
	request.Header.Set("Authorization", "token "+h.Token)
	response := httptest.NewRecorder()
	h.Mux.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "squash") {
		t.Fatalf("GraphQL status=%d body=%s", response.Code, response.Body.String())
	}
	after, err := h.Svc.Git.HeadSHA(ctx, repo.FullName, "main")
	if err != nil || after != before {
		t.Fatalf("disabled squash moved base before=%s after=%s err=%v", before, after, err)
	}

	bodyPayload, _ := json.Marshal(map[string]any{
		"query": `mutation($input:UpdatePullRequestInput!){updatePullRequest(input:$input){pullRequest{body}}}`,
		"variables": map[string]any{"input": map[string]any{
			"pullRequestId": "PullRequest_" + strconv.FormatUint(uint64(pr.ID), 10),
			"body":          "edited from file\n",
		}},
	})
	bodyRequest := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(bodyPayload))
	bodyRequest.Header.Set("Authorization", "token "+h.Token)
	bodyResponse := httptest.NewRecorder()
	h.Mux.ServeHTTP(bodyResponse, bodyRequest)
	if bodyResponse.Code != 200 || !strings.Contains(bodyResponse.Body.String(), "edited from file") {
		t.Fatalf("update body status=%d body=%s", bodyResponse.Code, bodyResponse.Body.String())
	}
}
