package server

import (
	"net/http"
	"path/filepath"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/githubappreceiver"
	"github.com/ngaut/agent-git-service/internal/integrations"
)

func initGitHubAppWebhook(cfg config.Config) (http.Handler, error) {
	if cfg.IntegrationsConfigFile == "" {
		return nil, nil
	}
	file, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	app := file.GitHubApp
	if !app.Enabled {
		return nil, nil
	}
	store, err := githubappreceiver.NewFileStore(filepath.Join(cfg.GitRepoDir, ".github-app-deliveries"))
	if err != nil {
		return nil, err
	}
	return (githubappreceiver.Receiver{AppID: app.AppID, Secret: app.WebhookSecret, Store: store}).Handler(), nil
}
