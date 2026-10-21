package githubintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func ValidateAttribution(ctx context.Context, cfg Config) (Config, error) {
	if err := cfg.Attribution.Validate(); err != nil {
		return cfg, err
	}
	if len(cfg.Attribution.Principals) == 0 {
		return cfg, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := NewHTTPClient(cfg.withDefaults().BaseURL, cfg.Token)
	status, data, err := c.request(ctx, http.MethodGet, "/user", nil)
	var user struct {
		Login string `json:"login"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &user) != nil || user.Login == "" {
		return cfg, fmt.Errorf("validate GitHub attribution credential: status %d", status)
	}
	cfg.TokenOwner = user.Login
	return cfg, nil
}

func githubWriteCoordinates(method, path string) (repo, op string, number int) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" {
		return "", "write", 0
	}
	repo = parts[1] + "/" + parts[2]
	if len(parts) > 4 {
		number, _ = strconv.Atoi(parts[4])
	}
	op = "edit_pr"
	if method == http.MethodPost && parts[3] == "pulls" {
		op = "create_pr"
	}
	if method == http.MethodPost && parts[3] == "issues" {
		op = "comment"
	}
	if method == http.MethodPatch {
		op = "close_pr"
	}
	return
}

func (i *Integration) AttributionEnabled() bool {
	return i != nil && len(i.cfg.Attribution.Principals) > 0
}
