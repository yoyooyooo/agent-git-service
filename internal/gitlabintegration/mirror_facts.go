package gitlabintegration

import (
	"errors"
	"net/url"
	"strings"
)

// MirrorConfiguration is the public subset of the instantiated configuration.
// It does not include credentials, file paths, or other repository mappings.
type MirrorConfiguration struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	MergeAuthority string `json:"merge_authority"`
}

// MirrorTarget is a configured same-ref destination, not a provider observation.
type MirrorTarget struct {
	ProjectPath string `json:"project_path"`
	Ref         string `json:"ref"`
}

type MirrorFacts struct {
	Ref           string               `json:"ref"`
	Configuration *MirrorConfiguration `json:"configuration"`
	Target        *MirrorTarget        `json:"target"`
}

// MirrorFacts reads only the running instance. A target means configuration can
// construct a PushRef destination; it does not prove credentials, connectivity,
// repository existence, or the current GitLab ref. No provider is contacted.
func (i *Integration) MirrorFacts(repoFullName, ref string) (MirrorFacts, error) {
	facts := MirrorFacts{Ref: strings.TrimSpace(ref)}
	if i == nil {
		return facts, nil
	}
	// Never publish URL credentials or a local transport path. Preserve the
	// actual configured HTTP(S) value rather than silently removing components.
	if i.cfg.BaseURL != "" {
		u, err := url.Parse(i.cfg.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return MirrorFacts{}, errors.New("gitlab integration: mirror facts require a credential-free HTTP(S) base URL")
		}
	}
	facts.Configuration = &MirrorConfiguration{Enabled: i.cfg.Enabled, BaseURL: i.cfg.BaseURL, MergeAuthority: i.cfg.MergeAuthority}
	branch, branchRef := branchFromRef(ref)
	if i.ready() != nil || !branchRef || !i.cfg.MirrorBranchEnabled(branch) {
		return facts, nil
	}
	mapping, mapped := i.cfg.targetFor(repoFullName)
	if !mapped {
		return facts, nil
	}
	// This is the same local construction used by PushRef. In particular, an
	// API-only project_id mapping is insufficient for a Git remote push.
	if _, err := i.cfg.authenticatedRemoteURL(mapping); err != nil {
		return facts, nil
	}
	facts.Target = &MirrorTarget{ProjectPath: mapping.projectPathOrDefault(repoFullName), Ref: "refs/heads/" + branch}
	return facts, nil
}
