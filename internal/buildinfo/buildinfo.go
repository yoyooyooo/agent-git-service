// Package buildinfo provides credential-free release identity without starting
// a runtime, reading configuration, opening a database or making a request.
package buildinfo

import (
	"encoding/json"
	"io"
	"runtime"
)

// Set by the release builder's linker flags. Development builds are explicit.
var Version = "development"
var Revision = "unknown"
var Tree = "unknown"

type Info struct {
	Schema   string `json:"schema"`
	Command  string `json:"command"`
	Version  string `json:"version"`
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
	GOOS     string `json:"goos"`
	GOARCH   string `json:"goarch"`
	Go       string `json:"go_version"`
}

func Current(command string) Info {
	return Info{"ags.build.v1", command, Version, Revision, Tree, runtime.GOOS, runtime.GOARCH, runtime.Version()}
}

// PrintVersion handles only an exact standalone version request. It has no
// configuration dependency and intentionally reveals no host or user metadata.
func PrintVersion(args []string, command string, out io.Writer) (bool, error) {
	if len(args) != 1 || (args[0] != "--version" && args[0] != "version") {
		return false, nil
	}
	return true, json.NewEncoder(out).Encode(Current(command))
}
