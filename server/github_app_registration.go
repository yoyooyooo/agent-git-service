package server

import (
	"net/http"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/githubappmanifest"
	"github.com/ngaut/agent-git-service/internal/integrations"
)

func initGitHubAppRegistrar(cfg config.Config) (*githubappmanifest.Registrar, error) {
	if cfg.IntegrationsConfigFile == "" {
		return nil, nil
	}
	file, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	app := file.GitHubApp
	if !app.RegistrationEnabled {
		return nil, nil
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	flow := &githubappmanifest.Flow{StatePath: app.RegistrationStateFile,
		Paths:     githubappmanifest.Paths{AppID: app.AppIDFile, PrivateKey: app.PrivateKeyFile, WebhookSecret: app.WebhookSecretFile},
		Exchanger: githubappmanifest.HTTPExchanger{BaseURL: app.APIBaseURL, Client: client}}
	return githubappmanifest.NewRegistrar(flow, app.RegistrationRedirectURL, app.APIBaseURL, client)
}
