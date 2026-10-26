package forgejointegration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/gittransport"
	"golang.org/x/crypto/ssh"
)

// ManagedSSHConfig is operator-owned routing and host trust, never a credential.
// Private keys live below the private operator/projection token directory.
type ManagedSSHConfig struct {
	URL            string `yaml:"url"`
	KnownHostsFile string `yaml:"known_hosts_file"`
}

var ErrMappedPushNotAttempted = errors.New("mapped Forgejo push was not attempted")

type managedKey struct {
	Instance string `json:"instance"`
	Identity string `json:"identity"`
	Login    string `json:"login"`
	Public   string `json:"public"`
}

func managedRoot(cfg Config) string {
	file := cfg.AuthorityPolicyTokenFile
	if file == "" {
		file = cfg.TokenFile
	}
	if file == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(cfg.BaseURL, "/")))
	return filepath.Join(filepath.Dir(file), "forgejo-managed-ssh", hex.EncodeToString(sum[:16]))
}

// The file lock serializes generation, registration, reconciliation and use
// across goroutines and processes sharing this instance's private directory.
func withManagedLock(ctx context.Context, cfg Config, create bool, fn func(string) error) error {
	root := managedRoot(cfg)
	if root == "" {
		return errors.New("managed Forgejo SSH requires a private token file directory")
	}
	if !create {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			return nil
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return errors.New("managed SSH directory unavailable")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("managed SSH directory must be private (0700)")
	}
	f, err := os.OpenFile(filepath.Join(root, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("managed SSH lock unavailable")
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return errors.New("managed SSH lock failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn(root)
}

func keyStem(instance, identity, login string) string {
	sum := sha256.Sum256([]byte(instance + "\x00" + identity + "\x00" + strings.ToLower(login)))
	return hex.EncodeToString(sum[:])
}

func privateRead(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("managed SSH file must be regular and private (0600)")
	}
	if info.Size() > 16384 {
		return nil, errors.New("managed SSH file too large")
	}
	b := make([]byte, info.Size())
	_, err = f.ReadAt(b, 0)
	return b, err
}

// Exclusive creation never overwrites an existing private key. The key is
// durable before any provider write; after a crash its public half is derived.
func loadManagedKey(root string, cfg Config, identity, login string) (managedKey, string, error) {
	k := managedKey{Instance: cfg.Attribution.Instance, Identity: identity, Login: login}
	path := filepath.Join(root, keyStem(k.Instance, identity, login)+".key")
	data, err := privateRead(path)
	if os.IsNotExist(err) {
		_, priv, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return k, "", errors.New("managed SSH generation failed")
		}
		block, e := ssh.MarshalPrivateKey(priv, "")
		if e != nil {
			return k, "", errors.New("managed SSH encoding failed")
		}
		data = pem.EncodeToMemory(block)
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if e != nil {
			return k, "", errors.New("managed SSH exclusive creation failed")
		}
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			return k, "", errors.New("managed SSH persistence failed")
		}
	} else if err != nil {
		return k, "", errors.New("managed SSH private key unavailable")
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return k, "", errors.New("managed SSH private key invalid")
	}
	k.Public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	metadata, _ := json.Marshal(k)
	// Metadata is persisted before registration, including after key-only crashes.
	if err = os.WriteFile(path+".json", metadata, 0600); err != nil {
		return k, "", errors.New("managed SSH registry unavailable")
	}
	return k, path, nil
}

type providerKey struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Title string `json:"title"`
}

func samePublic(a, b string) bool {
	aa := strings.Fields(a)
	bb := strings.Fields(b)
	return len(aa) >= 2 && len(bb) >= 2 && aa[0] == bb[0] && aa[1] == bb[1]
}
func managedTitle(k managedKey) string { return "ags-managed:" + k.Instance + ":" + k.Identity }

var (
	errManagedKeyInvalidResponse = errors.New("managed SSH public key readback invalid response")
	errManagedKeyInventoryBound  = errors.New("managed SSH key inventory exceeded bound")
)

func listManagedKeys(ctx context.Context, c *HTTPClient, login string) ([]providerKey, error) {
	keys, err := listManagedKeysEvidence(ctx, c, login)
	// Provisioning historically treats any page's 404 as an empty inventory.
	// Diagnostics retain that status without changing registration or sync.
	var providerErr *ProviderHTTPError
	if errors.As(err, &providerErr) && providerErr.Status == http.StatusNotFound {
		return nil, nil
	}
	// Provisioning retains its prior failure contract. Typed probe evidence must
	// not silently change its callers' classification or recovery policy.
	if err != nil && !errors.Is(err, errManagedKeyInventoryBound) {
		return nil, errors.New("managed SSH public key readback failed")
	}
	return keys, err
}

func listManagedKeysEvidence(ctx context.Context, c *HTTPClient, login string) ([]providerKey, error) {
	var all []providerKey
	for page := 1; page <= 100; page++ {
		status, data, err := c.rawRequest(ctx, http.MethodGet, apiPath("/api/v1/users/%s/keys", login)+fmt.Sprintf("?limit=50&page=%d", page), nil, "")
		if err != nil {
			return nil, gittransport.SafeError(err)
		}
		if status != http.StatusOK {
			return nil, &ProviderHTTPError{Status: status, Method: http.MethodGet, RouteKind: "user_keys"}
		}
		var keys []providerKey
		if json.Unmarshal(data, &keys) != nil {
			return nil, errManagedKeyInvalidResponse
		}
		all = append(all, keys...)
		if len(keys) < 50 {
			return all, nil
		}
	}
	return nil, errManagedKeyInventoryBound
}
func ensureManagedKey(ctx context.Context, c *HTTPClient, k managedKey) error {
	keys, err := listManagedKeys(ctx, c, k.Login)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if samePublic(key.Key, k.Public) {
			if key.Title != managedTitle(k) {
				return errors.New("managed SSH key ownership conflicts")
			}
			return nil
		}
	}
	status, _, writeErr := c.rawRequest(ctx, http.MethodPost, apiPath("/api/v1/admin/users/%s/keys", k.Login), map[string]any{"title": managedTitle(k), "key": k.Public, "read_only": false}, "")
	// Unknown creation is read back once, never blindly replayed.
	keys, err = listManagedKeys(ctx, c, k.Login)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if samePublic(key.Key, k.Public) && key.Title == managedTitle(k) {
			return nil
		}
	}
	if writeErr != nil || status != 201 {
		return errors.New("managed SSH public key registration failed")
	}
	return errors.New("managed SSH registered key not observed")
}

// ReconcileManagedSSH revokes only locally recorded public keys whose mapping
// disappeared/changed. Retained private files allow review and later reuse.
// An absent/empty map with no prior managed state makes zero provider requests.
func ReconcileManagedSSH(ctx context.Context, cfg Config) error {
	if managedRoot(cfg) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return withManagedLock(ctx, cfg, false, func(root string) error {
		entries, err := filepath.Glob(filepath.Join(root, "*.key.json"))
		if err != nil {
			return err
		}
		c := NewHTTPClient(cfg.BaseURL, cfg.Token)
		for _, path := range entries {
			data, err := privateRead(path)
			var k managedKey
			if err != nil || json.Unmarshal(data, &k) != nil || filepath.Base(path) != keyStem(k.Instance, k.Identity, k.Login)+".key.json" {
				return errors.New("managed SSH registry invalid")
			}
			if k.Instance != cfg.Attribution.Instance {
				continue
			}
			if cfg.Attribution.Principals[k.Identity].Forgejo == k.Login {
				continue
			}
			keys, err := listManagedKeys(ctx, c, k.Login)
			if err != nil {
				return err
			}
			for _, key := range keys {
				if samePublic(key.Key, k.Public) {
					status, _, err := c.rawRequest(ctx, http.MethodDelete, apiPath("/api/v1/admin/users/%s/keys/%s", k.Login, strconv.FormatInt(key.ID, 10)), nil, "")
					if err != nil || (status != 204 && status != 404) {
						return errors.New("managed SSH revocation failed")
					}
				}
			}
			keys, err = listManagedKeys(ctx, c, k.Login)
			if err != nil {
				return err
			}
			for _, key := range keys {
				if samePublic(key.Key, k.Public) {
					return errors.New("managed SSH revocation not observed")
				}
			}
		}
		return nil
	})
}

func (i *Integration) attributedPush(ctx context.Context, req PushRequest, push GitPusher) (result error) {
	if actor := attribution.FromContext(ctx); actor.RequestID == "" {
		ctx = attribution.WithActor(ctx, actor)
	}
	dispatched := false
	selectedPrincipal := unknownPrincipal()
	defer func() {
		if dispatched {
			status := "success"
			if result != nil {
				status = ClassifyProjectionError("", "", result).Type
			}
			diagnostic := projectionDiagnostic(ctx, req, selectedPrincipal, result)
			slog.InfoContext(ctx, "Forgejo projection push", "diagnostic", diagnostic, "authenticated_login", selectedPrincipal.Login, "status", status)
		}
		if result != nil && !dispatched {
			result = fmt.Errorf("%w: %w", ErrMappedPushNotAttempted, result)
		}
	}()
	actor := attribution.FromContext(ctx)
	login := i.cfg.Attribution.Principals[actor.Login].Forgejo
	if !i.cfg.Attribution.Enabled() || login == "" {
		// Resolve the selected credential, not the configured bot name. This is
		// push-scoped diagnostics, never a startup/readiness dependency.
		identityCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		selectedPrincipal = i.pushPrincipal(identityCtx, req)
		cancel()
		dispatched = true
		err := push(ctx, req)
		status := 0
		if err != nil {
			status = 1
		}
		attribution.Log(ctx, i.cfg.Attribution, "forgejo", "push_ref", req.Repo.Owner+"/"+req.Repo.Repo, i.cfg.TokenOwner, "", "bot_only", 0, status, false, "")
		return err
	}
	authorize := func() error {
		if actor.UserID == 0 || i.cfg.Attribution.Authorize == nil {
			return ErrAttributionAuthorizationRequired
		}
		return i.cfg.Attribution.Authorize(ctx, actor, actor.Repo, "push_ref", actor.PR)
	}
	if err := authorize(); err != nil {
		return err
	}
	target, targetErr := i.cfg.requireTargetFor(actor.Repo)
	if targetErr != nil || !target.Enabled || target.Owner != req.Repo.Owner || target.Repo != req.Repo.Repo {
		return errors.New("managed SSH push target does not match admitted AGS repository")
	}
	if i.cfg.Attribution.Instance == "" {
		return errors.New("managed SSH requires attribution.instance")
	}
	endpoint, err := url.Parse(i.cfg.ManagedSSH.URL)
	if err != nil || endpoint.Scheme != "ssh" || endpoint.Hostname() == "" || endpoint.User == nil || endpoint.User.Username() != "git" || endpoint.Path != "" && endpoint.Path != "/" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("managed SSH requires a credential-free ssh://git@host:port endpoint")
	}
	if _, has := endpoint.User.Password(); has {
		return errors.New("managed SSH endpoint cannot contain a password")
	}
	if !filepath.IsAbs(i.cfg.ManagedSSH.KnownHostsFile) {
		return errors.New("managed SSH requires an absolute known_hosts file")
	}
	endpoint.Path = "/" + req.Repo.Owner + "/" + req.Repo.Repo + ".git"
	req.RemoteURL = endpoint.String()
	c := NewHTTPClient(i.cfg.BaseURL, i.cfg.Token)
	err = withManagedLock(ctx, i.cfg, true, func(root string) error {
		if err := authorize(); err != nil {
			return err
		}
		k, path, err := loadManagedKey(root, i.cfg, actor.Login, login)
		if err != nil {
			return err
		}
		if err := ensureManagedKey(ctx, c, k); err != nil {
			return err
		}
		// Project ACL only after current AGS admission, before native user Git.
		permission, err := c.CollaboratorPermission(ctx, req.Repo.Owner, req.Repo.Repo, login)
		if err != nil {
			return err
		}
		if !forgejoPermissionAtLeastWrite(permission) && permission != "maintain" {
			mapping, ok := i.cfg.RepoMap[actor.Repo]
			if !ok || mapping.Owner != req.Repo.Owner || mapping.Repo != req.Repo.Repo || mapping.Enabled != nil && !*mapping.Enabled {
				return errors.New("managed SSH requires explicit repository mapping for ACL projection")
			}
			if err := authorize(); err != nil {
				return err
			}
			status, _, err := c.rawRequest(ctx, http.MethodPut, apiPath("/api/v1/repos/%s/%s/collaborators/%s", req.Repo.Owner, req.Repo.Repo, login), map[string]any{"permission": "write"}, "")
			granted := err == nil && status >= 200 && status < 300
			attribution.Log(ctx, i.cfg.Attribution, "forgejo", "collaborator_grant", req.Repo.Owner+"/"+req.Repo.Repo, i.cfg.TokenOwner, "", "bot_only", 0, status, granted, "", login)
			if !granted {
				return errors.New("managed SSH permission projection failed")
			}
		}
		if err := authorize(); err != nil {
			return err
		}
		req.SSH = &gittransport.SSHConfig{IdentityFile: path, KnownHostsFile: i.cfg.ManagedSSH.KnownHostsFile}
		// This probe never creates keys or grants ACLs. The existing legacy
		// provisioning above is unchanged and is not an attestation fallback.
		if i.nativePush {
			identityCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			selectedPrincipal = selectedSSHPrincipal(identityCtx, c, k)
			cancel()
		}
		dispatched = true
		return push(ctx, req)
	})
	status := 0
	if err != nil {
		status = 1
	}
	attribution.Log(ctx, i.cfg.Attribution, "forgejo", "push_ref", req.Repo.Owner+"/"+req.Repo.Repo, i.cfg.TokenOwner, "", "ssh", 0, status, false, "", login)
	return err
}
