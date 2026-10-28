package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingRunner struct {
	commands []command
	failAt   int
	output   string
}

func (r *recordingRunner) Run(_ context.Context, c command) error {
	r.commands = append(r.commands, c)
	if c.Output != nil {
		_, _ = io.WriteString(c.Output, r.output)
	}
	if r.failAt == len(r.commands) {
		return errors.New("exit status 3")
	}
	return nil
}

func TestExplicitSnapshotMustMatchHostJobAndSource(t *testing.T) {
	m, r := fakeManager(t, testConfig(t))
	r.output = `{"hostname":"test-server","tags":["backup-manager:server","source:db:app"]}`
	if _, err := m.snapshotSelection(context.Background(), "12345678", "files"); err == nil {
		t.Fatal("explicit DB snapshot accepted as files")
	}
	if _, err := m.snapshotSelection(context.Background(), "12345678", "db:app"); err != nil {
		t.Fatal(err)
	}
	r.output = `{"hostname":"other-server","tags":["backup-manager:server","source:db:app"]}`
	if _, err := m.snapshotSelection(context.Background(), "12345678", "db:app"); err == nil {
		t.Fatal("foreign host accepted")
	}
}

func fakeManager(t *testing.T, c Config) (*manager, *recordingRunner) {
	t.Helper()
	m, err := newManager(c, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{}
	m.runner = r
	return m, r
}

func hasPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}

func TestBackupStreamsWithoutSecretsInArguments(t *testing.T) {
	c := testConfig(t)
	c.Files.Paths = []string{t.TempDir()}
	c.Files.Exclude = []string{"*.tmp"}
	c.Postgres.Databases = []Database{{Name: "app", Database: "app db", Host: "localhost", User: "backup", Password: "db-secret", Port: 5432}}
	m, r := fakeManager(t, c)
	if err := m.backup(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(r.commands) != 2 {
		t.Fatalf("commands: %v", r.commands)
	}
	files, db := r.commands[0], r.commands[1]
	for _, cmd := range r.commands {
		joined := strings.Join(cmd.Args, " ")
		if strings.Contains(joined, "secret") || strings.Contains(joined, "--stdin ") {
			t.Fatalf("unsafe argv: %s", joined)
		}
		if !strings.Contains(joined, "--no-cache") || !hasPair(cmd.Args, "--pack-size", "16") {
			t.Fatal("missing resource flags")
		}
	}
	if !hasPair(files.Args, "--exclude", "*.tmp") || !hasPair(files.Args, "--exclude", c.Repository.URL) {
		t.Fatal("missing exclusions")
	}
	if !hasPair(db.Args, "--stdin-filename", "postgres-app.dump") || !hasPair(db.Args, "--", "pg_dump") ||
		!strings.Contains(strings.Join(db.Args, " "), "--stdin-from-command") {
		t.Fatalf("unsafe dump: %v", db.Args)
	}
	if envValue(db.Env, "PGPASSWORD") != "db-secret" || envValue(db.Env, "PGDATABASE") != "app db" {
		t.Fatal("missing PG environment")
	}
	if envValue(files.Env, "PGPASSWORD") != "" {
		t.Fatal("DB password leaked to file backup")
	}
	if envValue(db.Env, "RESTIC_PASSWORD") != c.Repository.Password {
		t.Fatal("missing repository password")
	}
	if envValue(db.Env, "TMPDIR") != c.Restic.TempDir || envValue(db.Env, "GOMEMLIMIT") != "512MiB" {
		t.Fatal("missing memory settings")
	}
}

func TestBackupStopsOnFailureAndChecksSources(t *testing.T) {
	c := testConfig(t)
	c.Files.Paths = []string{filepath.Join(t.TempDir(), "missing")}
	m, r := fakeManager(t, c)
	if err := m.backup(context.Background(), false); err == nil || len(r.commands) != 0 {
		t.Fatal("missing source accepted")
	}
	c.Files.Paths = []string{t.TempDir()}
	c.Postgres.Databases = []Database{{Name: "app", Database: "app"}, {Name: "other", Database: "other"}}
	for _, failAt := range []int{1, 2} {
		m, r = fakeManager(t, c)
		r.failAt = failAt
		if err := m.backup(context.Background(), false); err == nil || len(r.commands) != failAt {
			t.Fatal("failure ignored or subsequent source executed")
		}
	}
}

func TestInheritedEnvironmentCannotOverrideConfiguration(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD_COMMAND", "evil-command")
	t.Setenv("RESTIC_REPOSITORY", "wrong-repo")
	t.Setenv("PGOPTIONS", "unwanted")
	t.Setenv("AWS_PROFILE", "unwanted")
	c := testConfig(t)
	m, _ := fakeManager(t, c)
	for _, key := range []string{"RESTIC_PASSWORD_COMMAND", "PGOPTIONS", "AWS_PROFILE"} {
		if envValue(m.env, key) != "" {
			t.Fatalf("inherited %s", key)
		}
	}
	if envValue(m.env, "RESTIC_REPOSITORY") != c.Repository.URL {
		t.Fatal("repository overridden")
	}
}

func TestRestoreGuardsAndSelection(t *testing.T) {
	m, r := fakeManager(t, testConfig(t))
	target := filepath.Join(t.TempDir(), "restore")
	o := restoreOptions{Snapshot: "latest", Source: "db:old-app", Target: target}
	if err := m.restore(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	a := r.commands[0].Args
	if !hasPair(a, "--tag", "backup-manager:server,source:db:old-app") || !strings.Contains(strings.Join(a, " "), "--verify") {
		t.Fatal("restore selection/verification missing")
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.restore(context.Background(), o); err == nil {
		t.Fatal("accepted nonempty target")
	}
	o.Overwrite = true
	if err := m.restore(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Target = "relative"
	if err := m.restore(context.Background(), o); err == nil {
		t.Fatal("accepted relative target")
	}
	o.Target, o.Snapshot = target, "--bad-option"
	if err := m.restore(context.Background(), o); err == nil {
		t.Fatal("accepted option as snapshot")
	}
}

func TestRetentionScopedAndRunTagsNotGrouped(t *testing.T) {
	c := testConfig(t)
	m, r := fakeManager(t, c)
	if err := m.forget(context.Background(), false, true); err == nil {
		t.Fatal("accepted empty retention")
	}
	m.config.Retention.Daily = 7
	if err := m.forget(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	a := r.commands[0].Args
	for _, pair := range [][2]string{{"--host", c.Host}, {"--tag", m.tag()}, {"--group-by", "host,paths"}, {"--keep-daily", "7"}} {
		if !hasPair(a, pair[0], pair[1]) {
			t.Fatalf("missing %v", pair)
		}
	}
}

func TestRedactionAcrossEveryWriteBoundary(t *testing.T) {
	input := "before top-secret-value and second-secret after"
	for split := 0; split <= len(input); split++ {
		var out bytes.Buffer
		w := redactor{dst: &out, secrets: []string{"top-secret-value", "second-secret", ""}}
		_, _ = w.Write([]byte(input[:split]))
		_, _ = w.Write([]byte(input[split:]))
		_ = w.flush()
		if out.String() != "before [REDACTED] and [REDACTED] after" {
			t.Fatalf("split %d: %q", split, out.String())
		}
	}
}
