// Package attribution carries projection provenance, never authorization.
package attribution

import (
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

type Principal struct {
	Forgejo string `yaml:"forgejo"`
	GitHub  string `yaml:"github"`
	// CommitEmail asserts existing Git identity linkage; never rewrites commits or provider users.
	CommitEmail string `yaml:"commit_email"`
}

type Config struct {
	Observer func(context.Context, Event) error `yaml:"-"`
	// Authorize is owned by AGS Service. A mapping or Actor is never a grant.
	Authorize  func(context.Context, Actor, string, string, int) error `yaml:"-"`
	Instance   string                                                  `yaml:"instance"`
	Principals map[string]Principal                                    `yaml:"principals"`
}

var loginPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

func (c Config) Validate() error {
	if c.Instance != "" && !loginPattern.MatchString(c.Instance) {
		return fmt.Errorf("invalid attribution.instance")
	}
	seen := map[string]string{}
	for login, p := range c.Principals {
		if !loginPattern.MatchString(login) {
			return fmt.Errorf("invalid attribution principal")
		}
		for _, target := range []string{p.Forgejo, p.GitHub} {
			if target != "" && !loginPattern.MatchString(target) {
				return fmt.Errorf("invalid attribution target for %s", login)
			}
		}
		if p.CommitEmail != "" {
			address, err := mail.ParseAddress(p.CommitEmail)
			if err != nil || address.Address != p.CommitEmail || strings.ContainsAny(p.CommitEmail, "\r\n") || p.Forgejo == "" {
				return fmt.Errorf("invalid attribution commit_email for %s", login)
			}
		}
		if p.Forgejo != "" {
			key := strings.ToLower(p.Forgejo)
			if prev, ok := seen[key]; ok {
				return fmt.Errorf("ambiguous Forgejo attribution: %s and %s", prev, login)
			}
			seen[key] = login
		}
	}
	return nil
}

func (c Config) Enabled() bool { return len(c.Principals) > 0 }

func (c Config) ForgejoLogins() []string {
	out := []string{}
	for _, p := range c.Principals {
		if p.Forgejo != "" {
			out = append(out, p.Forgejo)
		}
	}
	sort.Strings(out)
	return out
}

func (c Config) AGSLogin(forgejo string) string {
	for login, p := range c.Principals {
		if p.Forgejo != "" && strings.EqualFold(p.Forgejo, forgejo) {
			return login
		}
	}
	return "forgejo"
}

type Actor struct {
	UserID    uint
	Login     string
	RequestID string
	Repo      string
	PR        int
}
type actorKey struct{}
type pushOpKey struct{}

func WithPushOp(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, pushOpKey{}, op)
}
func PushOp(ctx context.Context) string {
	if op, _ := ctx.Value(pushOpKey{}).(string); op != "" {
		return op
	}
	return "push_ref"
}

func WithActor(ctx context.Context, actor Actor) context.Context {
	if actor.RequestID == "" {
		actor.RequestID = uuid.NewString()
	}
	return context.WithValue(ctx, actorKey{}, actor)
}
func FromContext(ctx context.Context) Actor {
	actor, _ := ctx.Value(actorKey{}).(Actor)
	return actor
}
func WithCoordinates(ctx context.Context, repo string, pr int) context.Context {
	actor := FromContext(ctx)
	actor.Repo, actor.PR = repo, pr
	return WithActor(ctx, actor)
}

// Mark replaces only a generated leading marker, not text inside the user's body.
func Mark(body string, actor Actor, target, login, reason string) string {
	for strings.HasPrefix(body, "AGS-Actor:") {
		_, rest, ok := strings.Cut(body, "\n")
		body = rest
		if !ok {
			break
		}
	}
	who := actor.Login
	if !loginPattern.MatchString(who) {
		who = "system"
	}
	detail := target + "-" + reason
	if login != "" {
		detail = target + "=" + login + ", " + detail
	}
	return fmt.Sprintf("AGS-Actor: %s (ags-user-%d, %s)\n%s", who, actor.UserID, detail, body)
}

func Log(ctx context.Context, cfg Config, target, op, repo, tokenOwner, sudo, mode string, pr, status int, grant bool, reason string, targetLogins ...string) {
	if !cfg.Enabled() {
		return
	}
	a := FromContext(ctx)
	targetLogin := ""
	if len(targetLogins) > 0 {
		targetLogin = targetLogins[0]
	}
	if cfg.Observer != nil {
		event := Event{TargetLogin: targetLogin, Actor: a, Target: target, Op: op, TargetRepo: repo, TargetPR: pr, SudoAs: sudo, Attribution: mode, GrantApplied: grant, Status: status, FallbackReason: reason}
		if err := cfg.Observer(ctx, event); err != nil {
			slog.WarnContext(ctx, "projection attribution audit unavailable", "target", target, "op", op)
		}
	}
	slog.InfoContext(ctx, "projection attribution",
		"ags_instance", cfg.Instance, "target", target, "op", op,
		"repo", a.Repo, "target_repo", repo, "ags_pr", a.PR, "target_pr", pr,
		"ags_user_id", a.UserID, "ags_login", a.Login, "token_owner", tokenOwner,
		"sudo_as", sudo, "attribution", mode, "grant_applied", grant,
		"status", status, "request_id", a.RequestID, "fallback_reason", reason, "target_login", targetLogin)
}

// MergeTrailer is used only when AGS creates a new merge/squash object.
func MergeTrailer(message, login string) string {
	if !loginPattern.MatchString(login) {
		return message
	}
	lines := strings.Split(message, "\n")
	out := []string{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "AGS-Actor:") {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n\nAGS-Actor: " + login
}

// Event is a bounded credential/body-free audit record.
type Event struct {
	TargetLogin    string `json:"target_login,omitempty"`
	Actor          Actor  `json:"actor"`
	Target         string `json:"target"`
	Op             string `json:"op"`
	TargetRepo     string `json:"target_repo"`
	TargetPR       int    `json:"target_pr"`
	SudoAs         string `json:"sudo_as"`
	Attribution    string `json:"attribution"`
	GrantApplied   bool   `json:"grant_applied"`
	Status         int    `json:"status"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}
