package service

import (
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestSkipForgejoProjectionDriftScanForForkRepos(t *testing.T) {
	parentID := uint(29)
	cases := []struct {
		name string
		repo db.Repository
		want bool
	}{
		{name: "normal repo", repo: db.Repository{FullName: "example-team/shipping-fixture"}, want: false},
		{name: "fork flag", repo: db.Repository{FullName: "lane-a/shipping-fixture", Fork: true}, want: true},
		{name: "parent id", repo: db.Repository{FullName: "ariel/shipping-fixture", ParentID: &parentID}, want: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipForgejoProjectionDriftScan(tt.repo); got != tt.want {
				t.Fatalf("skipForgejoProjectionDriftScan() = %v, want %v", got, tt.want)
			}
		})
	}
}
