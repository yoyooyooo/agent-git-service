package server

import (
	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/integrations"
	"path/filepath"
)

func initCIBackends(cfg config.Config) (*cibackend.Registry, error) {
	if cfg.IntegrationsConfigFile == "" {
		return nil, nil
	}
	file, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	return cibackend.New(file.CI, filepath.Dir(cfg.IntegrationsConfigFile))
}
