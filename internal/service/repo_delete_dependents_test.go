package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/wikicatalog"
	"gorm.io/gorm"
)

// Keep the fixture tied to the migrated catalog: a new repository/PR FK must
// get a populated fixture and an explicit deletion/isolation assertion.
func TestDeleteRepoCascadeAllDependents(t *testing.T) {
	for _, backend := range []string{"sqlite", "tidb"} {
		t.Run(backend, func(t *testing.T) {
			for _, rollback := range []bool{false, true} {
				t.Run(fmt.Sprintf("rollback_%t", rollback), func(t *testing.T) {
					var database *gorm.DB
					if backend == "sqlite" {
						var err error
						database, err = db.Init("sqlite:" + filepath.Join(t.TempDir(), "cascade.db") + "?_foreign_keys=on")
						if err != nil {
							t.Fatal(err)
						}
						pool, err := database.DB()
						if err != nil {
							t.Fatal(err)
						}
						pool.SetMaxOpenConns(1)
						t.Cleanup(func() { _ = pool.Close() })
						var enabled int
						if err := database.Raw("PRAGMA foreign_keys").Scan(&enabled).Error; err != nil || enabled != 1 {
							t.Fatalf("foreign keys must be enabled: %d, %v", enabled, err)
						}
					} else {
						var cleanup func()
						database, cleanup = openMigratedServiceTestDB(t)
						defer cleanup()
						var enabled int
						if err := database.Raw("SELECT @@foreign_key_checks").Scan(&enabled).Error; err != nil || enabled != 1 {
							t.Fatalf("foreign keys must be enabled: %d, %v", enabled, err)
						}
					}
					testDeleteRepoAllDependents(t, database, rollback)
				})
			}
		})
	}
}

type cascadeRow struct {
	model    any
	key      map[string]any
	snapshot any
}

type cascadeFixture struct {
	database *gorm.DB
	rows     []cascadeRow
	repo     db.Repository
	pr       db.PullRequest
	session  db.DelegatedAgentSession
}

func (f *cascadeFixture) add(t *testing.T, model any) {
	t.Helper()
	if err := f.database.Omit("Embedding").Create(model).Error; err != nil {
		t.Fatalf("seed %T: %v", model, err)
	}
	stmt := &gorm.Statement{DB: f.database}
	if err := stmt.Parse(model); err != nil {
		t.Fatal(err)
	}
	key := make(map[string]any)
	for _, field := range stmt.Schema.PrimaryFields {
		value, _ := field.ValueOf(context.Background(), reflect.Indirect(reflect.ValueOf(model)))
		key[field.DBName] = value
	}
	if len(key) == 0 {
		t.Fatalf("fixture has no primary key: %T", model)
	}
	snapshot := reflect.New(reflect.TypeOf(model).Elem()).Interface()
	if err := f.database.Where(key).Take(snapshot).Error; err != nil {
		t.Fatal(err)
	}
	f.rows = append(f.rows, cascadeRow{model, key, snapshot})
}

func (f *cascadeFixture) assertRows(t *testing.T, remain bool) {
	t.Helper()
	for _, row := range f.rows {
		got := reflect.New(reflect.TypeOf(row.model).Elem()).Interface()
		err := f.database.Where(row.key).Take(got).Error
		if !remain {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Errorf("%T %v must be removed: %v", row.model, row.key, err)
			}
		} else if err != nil || !reflect.DeepEqual(got, row.snapshot) {
			t.Errorf("%T %v changed: error=%v\ngot=%+v\nwant=%+v", row.model, row.key, err, got, row.snapshot)
		}
	}
}

func seedCascadeFixture(t *testing.T, database *gorm.DB, owner db.User, name string) *cascadeFixture {
	t.Helper()
	f := &cascadeFixture{database: database}
	f.repo = db.Repository{OwnerID: owner.ID, Name: name, FullName: owner.Login + "/" + name, DefaultBranch: "main"}
	f.add(t, &f.repo)
	r := f.repo.ID
	now := time.Now().UTC()
	f.session = db.DelegatedAgentSession{
		ID: name + "-session", RepositoryID: r, PrincipalUserID: owner.ID,
		CredentialHash: name + "-synthetic-hash", Issuer: "fixture", AssertionVersion: 1,
		AssertionPurpose: "fixture", AssertionJTI: name, AssertionAudience: "fixture",
		TargetInstance: "fixture", GrantedCapabilities: []string{"repo.read"}, PolicyVersion: "fixture",
		PolicySnapshotHash: "fixture", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	f.add(t, &f.session)
	milestone := db.Milestone{RepositoryID: r, Number: 1, Title: name, CreatorID: owner.ID}
	f.add(t, &milestone)
	issue := db.Issue{RepositoryID: r, Number: 1, Title: name, AuthorID: owner.ID, MilestoneID: &milestone.ID}
	f.add(t, &issue)
	comment := db.IssueComment{RepositoryID: r, IssueNumber: 1, AuthorID: owner.ID, Body: "fixture"}
	f.add(t, &comment)
	label := db.Label{RepositoryID: r, Name: "fixture", Color: "ffffff"}
	f.add(t, &label)
	f.add(t, &db.IssueEvent{IssueID: issue.ID, EventType: "opened", ActorLogin: owner.Login})
	f.add(t, &db.LinkedBranch{RepositoryID: r, IssueID: issue.ID, BranchName: "feature"})
	f.add(t, &db.RepoIncident{RepositoryID: r, IssueID: issue.ID, Source: "fixture", Type: "failure", AggregateKey: name})
	f.add(t, &db.IssueReference{SourceType: "issue_comment", SourceRepositoryID: r, TargetRepositoryID: r, SourceCommentID: &comment.ID, TargetNumber: 1})
	f.pr = db.PullRequest{RepositoryID: r, HeadRepositoryID: r, Number: 2, Title: name, AuthorID: owner.ID, AgentSessionID: &f.session.ID, MilestoneID: &milestone.ID, HeadRef: "feature", BaseRef: "main"}
	f.add(t, &f.pr)
	f.seedPRDependents(t, f.pr, owner, name)
	if err := database.Model(&issue).Association("Labels").Append(&label); err != nil {
		t.Fatal(err)
	}
	if err := database.Model(&f.pr).Association("Labels").Append(&label); err != nil {
		t.Fatal(err)
	}
	f.add(t, &db.WikiPageLabel{RepositoryID: r, Slug: "home", LabelID: label.ID})
	webhook := db.Webhook{RepositoryID: r, Name: "fixture"}
	f.add(t, &webhook)
	delivery := db.HookDelivery{RepositoryID: r, WebhookID: webhook.ID, GUID: name, Event: "push"}
	f.add(t, &delivery)
	f.add(t, &db.HookDelivery{RepositoryID: r, WebhookID: webhook.ID, ParentDeliveryID: &delivery.ID, GUID: name + "-retry", Event: "push", Redelivery: true})
	deployment := db.Deployment{RepositoryID: r, CreatorID: owner.ID, Ref: "main"}
	f.add(t, &deployment)
	f.add(t, &db.DeploymentStatus{DeploymentID: deployment.ID, CreatorID: owner.ID, State: "success"})
	release := db.Release{RepositoryID: r, AuthorID: owner.ID, TagName: "v1"}
	f.add(t, &release)
	f.add(t, &db.ReleaseAsset{ReleaseID: release.ID, Name: "fixture"})
	team := db.Team{OrganizationID: owner.ID, Name: name, Slug: name}
	// Teams are owner-scoped and must survive repository deletion.
	if err := database.Create(&team).Error; err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{
		&db.TeamRepository{TeamID: team.ID, RepositoryID: r, Permission: "read"},
		&db.IssuePRNumberCounter{RepositoryID: r, NextNumber: 3},
		&db.RepoRedirect{RepoID: r, OldFullName: owner.Login + "/old-" + name},
		&db.DeployKey{RepositoryID: r, Title: name},
		&db.Star{RepositoryID: r, UserID: owner.ID},
		&db.CommitStatus{RepositoryID: r, CreatorID: owner.ID, CommitSHA: "fixture", State: "success"},
		&db.BranchProtection{RepositoryID: r, BranchName: "main"},
		&db.DependabotAlert{RepositoryID: r, Number: 1, State: "open"},
		&db.RepositoryInvitation{RepositoryID: r, InviteeID: owner.ID, InviterID: owner.ID},
		&db.Collaborator{RepositoryID: r, UserID: owner.ID, Permission: "admin"},
		&db.Notification{RepositoryID: r, UserID: owner.ID, SubjectID: f.pr.ID, Type: "pull_request", SubjectType: NotificationSubjectPullRequest},
		&db.ExternalEvent{RepositoryID: r, EventKey: name, Source: "fixture", Type: "failure", AggregateKey: name, OccurredAt: now, AcceptedAt: now},
		&db.CIResource{RepositoryID: r, Namespace: "fixture", ExternalRepository: name, Kind: "run", ExternalID: "1"},
		&db.Environment{RepositoryID: r, Name: "test"},
		&db.PagesConfig{RepositoryID: r, SourceBranch: "main"},
		&db.PagesBuild{RepositoryID: r, Status: "queued"},
		&db.ProjectRepoLink{ProjectID: 1, RepositoryID: r},
		&db.ProjectionEvent{RepositoryID: r, Provider: "forgejo", Type: "fixture", Status: "active", RepoFullName: f.repo.FullName, Ref: "refs/heads/main", OccurredAt: now},
		&db.ProjectionRefState{RepositoryID: r, Provider: "forgejo", Type: "fixture", Status: "active", RepoFullName: f.repo.FullName, Ref: "refs/heads/main", FirstSeenAt: now, LastSeenAt: now},
		&db.RepoFlowEnvProjection{RepositoryID: r, RepoFullName: f.repo.FullName, Env: "test", SourceRef: "refs/heads/main", SourceSHA: "fixture", State: "projected", TriggeredAt: now},
		&db.WikiPageIndex{RepositoryID: r, Slug: "home", HeadBlobSHA: "fixture", HeadCommitSHA: "fixture", UpdatedAt: now},
		&db.WikiIndexState{RepositoryID: r, IndexedCommitSHA: "fixture", UpdatedAt: now},
		&db.WikiBacklink{RepositoryID: r, SrcSlug: "home", DstSlug: "other", UpdatedAt: now},
		&db.WikiPageHistory{RepositoryID: r, Slug: "home", CommitSHA: "fixture", Message: "fixture", CommittedAt: now},
		&db.WikiSearchDocument{RepositoryID: r, Slug: "home", Title: "fixture", RevisionSHA: "fixture"},
		&db.WikiSearchProjectionTask{RepositoryID: r, Slug: "home", Kind: "lexical", Generation: 1},
		&db.WikiGitRepairObligation{RepositoryID: r, HeadSHA: "fixture"},
		&db.WikiCompactionJob{ID: name + "-compact", RepositoryID: r, Status: WikiCompactionJobSucceeded},
	} {
		f.add(t, model)
	}
	changeset := db.WikiChangeset{RepositoryID: r, Source: "rest", SynthCommitSHA: "fixture", CommittedAt: now}
	f.add(t, &changeset)
	page := db.WikiPage{RepositoryID: r, Slug: "home", HeadBlobSHA: "shared-blob", BodySize: wikicatalog.MaxBodyInlineBytes + 1, HeadRevisionID: 1, HeadChangesetID: changeset.ChangesetID}
	f.add(t, &page)
	f.add(t, &db.WikiPageRevision{PageID: page.PageID, RevisionID: 1, ChangesetID: changeset.ChangesetID, BlobSHA: "shared-blob", BodySize: page.BodySize, SlugAtRev: "home", CommitSHA: "fixture", Op: "create", CommittedAt: now})
	f.add(t, &db.WikiPageLink{RepositoryID: r, SrcPageID: page.PageID, DstSlug: "other"})
	f.add(t, &db.WikiRepoHead{RepositoryID: r, HeadChangesetID: changeset.ChangesetID, UpdatedAt: now})
	return f
}

func (f *cascadeFixture) seedPRDependents(t *testing.T, pr db.PullRequest, owner db.User, name string) {
	t.Helper()
	review := db.PullRequestReview{PullRequestID: pr.ID, AuthorLogin: owner.Login, State: "APPROVED"}
	f.add(t, &review)
	f.add(t, &db.PRReviewComment{PullRequestID: pr.ID, PullRequestReviewID: &review.ID, AuthorLogin: owner.Login, Body: "fixture"})
	f.add(t, &db.ReviewRequest{PullRequestID: pr.ID, Login: owner.Login})
	f.add(t, &db.PullRequestProjection{RepositoryID: pr.RepositoryID, PullRequestID: pr.ID, Provider: "forgejo", ExternalRepo: name, ExternalNumber: pr.Number})
	job := db.PullRequestProjectionJob{RepositoryID: pr.RepositoryID, PullRequestID: pr.ID, Provider: "forgejo", RepoFullName: name, AGSPRNumber: pr.Number}
	f.add(t, &job)
	f.add(t, &db.PullRequestProjectionJobAttempt{JobID: job.ID, Attempt: 1, Phase: "queued", Status: "failed"})
	f.add(t, &db.PullRequestMulticaLink{RepositoryID: pr.RepositoryID, PullRequestID: pr.ID, Workspace: "fixture", IssueID: name, IssueKey: name, Source: db.MulticaLinkSourceMarker})
	run := db.ClientRunSession{ID: name + "-run", UserID: owner.ID, CredentialHash: name + "-run-hash", ContextJSON: "{}", AssociationStatus: "linked", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := f.database.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	f.add(t, &db.ClientRunLink{RunID: run.ID, RepositoryID: pr.RepositoryID, PullRequestID: pr.ID})
	f.add(t, &db.PullRequestActionIntent{ID: name + "-intent", IdempotencyKey: name, Action: "pr.rebase", State: "failed", PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AGSPRNumber: pr.Number, ExpiresAt: time.Now().UTC()})
}

func testDeleteRepoAllDependents(t *testing.T, database *gorm.DB, rollback bool) {
	t.Helper()
	owner := db.User{Login: "cascade-owner", Type: db.TypeUser}
	if err := database.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	remove := seedCascadeFixture(t, database, owner, "remove")
	keep := seedCascadeFixture(t, database, owner, "keep")
	for i, pair := range [][2]uint{{remove.repo.ID, keep.repo.ID}, {keep.repo.ID, remove.repo.ID}} {
		pr := db.PullRequest{RepositoryID: pair[0], HeadRepositoryID: pair[1], Number: 10 + i, Title: "cross", AuthorID: owner.ID, HeadRef: "feature", BaseRef: "main"}
		remove.add(t, &pr)
		remove.seedPRDependents(t, pr, owner, fmt.Sprintf("cross-%d", i))
		label := db.Label{RepositoryID: pair[0], Name: fmt.Sprintf("cross-%d", i)}
		if err := database.Create(&label).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.Model(&pr).Association("Labels").Append(&label); err != nil {
			t.Fatal(err)
		}
		remove.add(t, &db.Notification{RepositoryID: pair[0], UserID: owner.ID, SubjectID: pr.ID, Type: "pull_request", SubjectType: NotificationSubjectPullRequest})
		prNumber := pr.Number
		remove.add(t, &db.IssueReference{SourceType: issueReferenceSourcePullRequestBody, SourceRepositoryID: pair[0], SourcePRNumber: &prNumber, TargetRepositoryID: pair[0], TargetNumber: 1})
	}
	remove.add(t, &db.IssueReference{SourceType: "issue", SourceRepositoryID: keep.repo.ID, TargetRepositoryID: remove.repo.ID, TargetNumber: 1})
	remove.add(t, &db.IssueReference{SourceType: "issue", SourceRepositoryID: remove.repo.ID, TargetRepositoryID: keep.repo.ID, TargetNumber: 1})
	fork := db.Repository{OwnerID: owner.ID, Name: "fork", FullName: owner.Login + "/fork", ParentID: &remove.repo.ID, Fork: true}
	if err := database.Create(&fork).Error; err != nil {
		t.Fatal(err)
	}
	blob := db.WikiBlobRef{BlobSHA: "shared-blob", Refcount: 2, Size: wikicatalog.MaxBodyInlineBytes + 1, FirstSeen: time.Now().UTC(), LastSeen: time.Now().UTC()}
	if err := database.Create(&blob).Error; err != nil {
		t.Fatal(err)
	}
	if database.Dialector.Name() == "sqlite" {
		assertCascadeFKFixtureCoverage(t, database, remove, keep)
	}

	// Association appends update their parent timestamps. Snapshot the complete
	// fixture only after all setup writes, immediately before deletion.
	for _, fixture := range []*cascadeFixture{remove, keep} {
		for _, row := range fixture.rows {
			if err := database.Where(row.key).Take(row.snapshot).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	svc := &Service{DB: database}
	ctx := ContextWithUser(context.Background(), owner)
	if rollback {
		const callback = "test:fail_final_repository_delete"
		injected := errors.New("injected final repository delete failure")
		if err := database.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "repositories" {
				tx.AddError(injected)
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer database.Callback().Delete().Remove(callback)
		if err := svc.DeleteRepo(ctx, remove.repo.FullName); !errors.Is(err, injected) {
			t.Fatalf("expected final-delete failure, got %v", err)
		}
		remove.assertRows(t, true)
	} else {
		if err := svc.DeleteRepo(ctx, remove.repo.FullName); err != nil {
			t.Fatalf("DeleteRepo with all dependents: %v", err)
		}
		remove.assertRows(t, false)
	}
	keep.assertRows(t, true)
	var gotFork db.Repository
	if err := database.First(&gotFork, fork.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rollback {
		if gotFork.ParentID == nil || *gotFork.ParentID != remove.repo.ID || !gotFork.Fork {
			t.Fatal("rollback did not restore fork parent")
		}
	} else if gotFork.ParentID != nil || gotFork.Fork {
		t.Fatal("fork was not detached")
	}
	if err := database.First(&blob, "blob_sha = ?", blob.BlobSHA).Error; err != nil {
		t.Fatal(err)
	}
	wantRefs := int64(1)
	if rollback {
		wantRefs = 2
	}
	if blob.Refcount != wantRefs {
		t.Fatalf("shared blob refcount = %d, want %d", blob.Refcount, wantRefs)
	}
	var joins int64
	if err := database.Table("pr_labels").Count(&joins).Error; err != nil {
		t.Fatal(err)
	}
	wantJoins := int64(1)
	if rollback {
		wantJoins = 4
	}
	if joins != wantJoins {
		t.Fatalf("PR label joins = %d, want %d", joins, wantJoins)
	}
	if database.Dialector.Name() == "sqlite" {
		var violations []map[string]any
		if err := database.Raw("PRAGMA foreign_key_check").Scan(&violations).Error; err != nil || len(violations) != 0 {
			t.Fatalf("foreign-key violations: %v, %v", violations, err)
		}
	}
}

func assertCascadeFKFixtureCoverage(t *testing.T, database *gorm.DB, fixtures ...*cascadeFixture) {
	t.Helper()
	tables, err := database.Migrator().GetTables()
	if err != nil {
		t.Fatal(err)
	}
	var inventory []string
	for _, table := range tables {
		var keys []struct {
			Table string
			From  string
		}
		if err := database.Raw("PRAGMA foreign_key_list(" + table + ")").Scan(&keys).Error; err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if key.Table != "repositories" && key.Table != "pull_requests" {
				continue
			}
			inventory = append(inventory, table+"."+key.From+" -> "+key.Table)
			for _, fixture := range fixtures {
				id := fixture.repo.ID
				if key.Table == "pull_requests" {
					id = fixture.pr.ID
				}
				// Fork-parent preservation has its own detach/rollback assertions.
				if table == "repositories" {
					continue
				}
				var count int64
				if err := database.Table(table).Where(key.From+" = ?", id).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count == 0 {
					t.Errorf("unpopulated FK fixture: %s.%s -> %s for %s", table, key.From, key.Table, fixture.repo.Name)
				}
			}
		}
	}
	sort.Strings(inventory)
	t.Logf("migrated repository/PR foreign keys (%d): %v", len(inventory), inventory)
}
