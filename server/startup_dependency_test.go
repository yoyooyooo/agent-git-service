package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	appdb "github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Subprocesses execute the real standalone Run path with an allowlisted
// environment. No parent integration configuration or credentials are inherited.
func TestStartupDependencyProcess(t *testing.T) {
	mode := os.Getenv("AGS_STARTUP_TEST_PROCESS")
	if mode == "" {
		return
	}
	if mode == "migration" {
		database, err := gorm.Open(sqlite.Open(os.Getenv("DB_DSN")), &gorm.Config{Logger: startupDDLInterruption{Interface: logger.Discard}})
		if err != nil {
			t.Fatal(err)
		}
		if err := appdb.Migrate(database); err != nil {
			t.Fatal(err)
		}
		t.Fatal("migration never reached the interruption boundary")
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	stop := make(chan struct{})
	go func() {
		<-signals
		close(stop)
	}()
	if err := Run(stop); err != nil {
		t.Fatal(err)
	}
}

type startupDDLInterruption struct{ logger.Interface }

func (l startupDDLInterruption) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sql, _ := fc()
	if err == nil && strings.HasPrefix(strings.ToUpper(sql), "CREATE TABLE") {
		if err := os.WriteFile(os.Getenv("AGS_STARTUP_DDL_MARKER"), []byte("committed DDL"), 0600); err != nil {
			panic(err)
		}
		// Model loss of the process after committed DDL, before Migrate returns.
		select {}
	}
}

type startupProcess struct {
	cmd  *exec.Cmd
	done chan error
	log  string
}

func startDependencyProcess(t *testing.T, root, dsn, port, mode string) *startupProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(root, "process-*.log")
	if err != nil {
		t.Fatal(err)
	}
	p := &startupProcess{cmd: exec.Command(executable, "-test.run=^TestStartupDependencyProcess$", "-test.v"), done: make(chan error, 1), log: logFile.Name()}
	p.cmd.Dir = root
	p.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root,
		"AGS_STARTUP_TEST_PROCESS=" + mode, "AGS_STARTUP_DDL_MARKER=" + filepath.Join(root, "ddl-committed"),
		"DB_DSN=" + dsn, "GIT_REPO_DIR=" + filepath.Join(root, "repos"),
		"PORT=" + port, "BASE_URL=http://127.0.0.1:" + port,
		"ENVIRONMENT=development", "LISTEN_MODE=production",
		"ADMIN_LOGIN=fixture-admin", "ADMIN_TOKEN=fixture-startup-token",
		"AGS_ALLOW_MISSING_PROJECTION_ALERTING=true",
	}
	p.cmd.Stdout, p.cmd.Stderr = logFile, logFile
	if err := p.cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait(); _ = logFile.Close() }()
	t.Cleanup(func() {
		// Kill is only a cleanup fallback for this test-owned subprocess.
		_ = p.cmd.Process.Kill()
	})
	return p
}

func waitStartupCondition(t *testing.T, p *startupProcess, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case err := <-p.done:
			body, _ := os.ReadFile(p.log)
			t.Fatalf("startup process exited early: %v\n%s", err, body)
		case <-deadline.C:
			body, _ := os.ReadFile(p.log)
			t.Fatalf("startup condition timed out\n%s", body)
		case <-tick.C:
		}
	}
}

func startupPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func stopDependencyProcess(t *testing.T, p *startupProcess, port string) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			body, _ := os.ReadFile(p.log)
			t.Fatalf("normal shutdown: %v\n%s", err, body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("normal shutdown did not drain workers and listeners")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("listener remains after shutdown: %v", err)
	}
	_ = ln.Close()
}

func startupFixtureDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return database
}

func runHealthyStartupCycles(t *testing.T, root, dsn string) {
	t.Helper()
	var previousUsers, previousTokens int64
	for cycle := 0; cycle < 2; cycle++ {
		port := startupPort(t)
		p := startDependencyProcess(t, root, dsn, port, "server")
		client := &http.Client{Timeout: 200 * time.Millisecond}
		waitStartupCondition(t, p, func() bool {
			response, err := client.Get("http://127.0.0.1:" + port + "/readyz")
			if err != nil {
				return false
			}
			defer response.Body.Close()
			return response.StatusCode == http.StatusOK
		})
		request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/api/v3/user", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer fixture-startup-token")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("authenticated read returned %d", response.StatusCode)
		}
		stopDependencyProcess(t, p, port)
		database := startupFixtureDB(t, dsn)
		var users, tokens, pending int64
		for model, count := range map[any]*int64{&appdb.User{}: &users, &appdb.Token{}: &tokens, &appdb.PullRequestProjectionJob{}: &pending} {
			if err := database.Model(model).Count(count).Error; err != nil {
				t.Fatal(err)
			}
		}
		if users == 0 || tokens == 0 || pending != 0 {
			t.Fatalf("unexpected startup facts users=%d tokens=%d jobs=%d", users, tokens, pending)
		}
		if cycle > 0 && (users != previousUsers || tokens != previousTokens) {
			t.Fatalf("restart duplicated seed rows users=%d/%d tokens=%d/%d", users, previousUsers, tokens, previousTokens)
		}
		previousUsers, previousTokens = users, tokens
		// All SQLite handles can write after shutdown; no retained transaction remains.
		if err := database.Exec("BEGIN EXCLUSIVE").Error; err != nil {
			t.Fatal(err)
		}
		if err := database.Exec("ROLLBACK").Error; err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(root, "repos"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("empty fixture acquired Git refs/artifacts: entries=%v err=%v", entries, err)
		}
	}
}

func TestStartupDependencyDBUnavailableThenRestart(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "database")
	if err := os.WriteFile(parent, []byte("unavailable database directory"), 0600); err != nil {
		t.Fatal(err)
	}
	dsn := "file:" + filepath.Join(parent, "primary.sqlite")
	p := startDependencyProcess(t, root, dsn, startupPort(t), "server")
	select {
	case err := <-p.done:
		body, _ := os.ReadFile(p.log)
		if err == nil || !strings.Contains(string(body), "db:") {
			t.Fatalf("unavailable DB did not fail startup: %v\n%s", err, body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unavailable DB did not fail promptly")
	}
	if _, err := os.Stat(filepath.Join(root, "repos")); !os.IsNotExist(err) {
		t.Fatalf("Git initialized before DB was available: %v", err)
	}
	if err := os.Rename(parent, parent+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	runHealthyStartupCycles(t, root, dsn)
}

func TestStartupDependencyInterruptedMigrationThenRestart(t *testing.T) {
	root := t.TempDir()
	dsn := "file:" + filepath.Join(root, "primary.sqlite")
	p := startDependencyProcess(t, root, dsn, startupPort(t), "migration")
	waitStartupCondition(t, p, func() bool { _, err := os.Stat(filepath.Join(root, "ddl-committed")); return err == nil })
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-p.done; err == nil {
		t.Fatal("interrupted migration process unexpectedly succeeded")
	}
	database := startupFixtureDB(t, dsn)
	if !database.Migrator().HasTable(&appdb.User{}) || database.Migrator().HasTable(&appdb.Repository{}) {
		t.Fatal("fault did not leave the expected committed partial schema")
	}
	var users int64
	if err := database.Model(&appdb.User{}).Count(&users).Error; err != nil || users != 0 {
		t.Fatalf("seed ran before migration completed: users=%d err=%v", users, err)
	}
	runHealthyStartupCycles(t, root, dsn)
}
