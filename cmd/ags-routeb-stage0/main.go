package main

import (
	"encoding/json"
	"os"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/routebstage0"
)

func main() {
	if handled, err := buildinfo.PrintVersion(os.Args[1:], "ags-routeb-stage0", os.Stdout); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := routebstage0.Execute(os.Args[1:]); err != nil {
		json.NewEncoder(os.Stderr).Encode(map[string]any{"schema": "ags.routeb-stage0-result.v1", "status": "blocked", "reason": routebstage0.ErrorCode(err)})
		os.Exit(78)
	}
}
