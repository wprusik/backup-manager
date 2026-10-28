//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func integrationManager(t *testing.T) *manager {
	t.Helper()
	if os.Getenv("BACKUP_MANAGER_INTEGRATION") != "1" {
		t.Skip("set BACKUP_MANAGER_INTEGRATION=1; see scripts/test-linux.sh")
	}
	if _, err := exec.LookPath("restic"); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	c.Restic.RequireTmpfs = true
	c.Restic.TempDir = t.TempDir() // test-linux.sh places TMPDIR on tmpfs.
	var logs strings.Builder
	m, err := newManager(c, &logs, &logs)
	if err != nil {
		t.Fatal(err)
	}
	// stdout and stderr are concurrent; separate buffers avoid a data race.
	var outLog, errLog bytes.Buffer
	m.runner = execRunner{Out: &outLog, Err: &errLog, Secrets: []string{c.Repository.Password}}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("restic stdout:\n%s\nrestic stderr:\n%s", outLog.String(), errLog.String())
		}
	})
	m.out = io.Discard
	if err := m.exec(context.Background(), nil, "init", "--repository-version", "2"); err != nil {
		t.Fatal(err)
	}
	return m
}

func runExternal(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, output)
	}
	return output
}

func TestIntegrationFilesMetadataIncrementalAndExcludes(t *testing.T) {
	m := integrationManager(t)
	ctx := context.Background()
	source := t.TempDir()
	file := filepath.Join(source, "data")
	if err := os.WriteFile(file, []byte("first version"), 0640); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(file, 12345, 23456); err != nil {
			t.Fatal(err)
		}
	}
	runExternal(t, "setfattr", "-n", "user.backup-test", "-v", "metadata", file)
	runExternal(t, "setfacl", "-m", "u:12346:r--", file)
	if err := os.Symlink("data", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "skip.tmp"), []byte("excluded"), 0600); err != nil {
		t.Fatal(err)
	}
	m.config.Files = Files{Paths: []string{source}, Exclude: []string{"*.tmp"}}
	before, _ := os.Stat(file)
	if err := m.backup(ctx, false); err != nil {
		t.Fatal(err)
	}
	// An unchanged second backup should reuse existing encrypted data packs.
	packFiles := func() int {
		n := 0
		_ = filepath.WalkDir(filepath.Join(m.config.Repository.URL, "data"), func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				n++
			}
			return err
		})
		return n
	}
	count := packFiles()
	if err := m.backup(ctx, false); err != nil {
		t.Fatal(err)
	}
	if packFiles() != count {
		t.Fatal("unchanged backup wrote new data packs")
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := m.restore(ctx, restoreOptions{Snapshot: "latest", Source: "files", Target: target}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(target, strings.TrimPrefix(file, "/"))
	data, err := os.ReadFile(restored)
	if err != nil || string(data) != "first version" {
		t.Fatalf("restored data: %q %v", data, err)
	}
	after, err := os.Stat(restored)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("mode or mtime changed")
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatal("UID/GID changed")
	}
	if got := string(runExternal(t, "getfattr", "--absolute-names", "--only-values", "-n", "user.backup-test", restored)); got != "metadata" {
		t.Fatalf("xattr: %q", got)
	}
	aclBefore := runExternal(t, "getfacl", "-cn", file)
	aclAfter := runExternal(t, "getfacl", "-cn", restored)
	if string(aclBefore) != string(aclAfter) {
		t.Fatalf("ACL changed: %s != %s", aclBefore, aclAfter)
	}
	link, err := os.Readlink(filepath.Join(filepath.Dir(restored), "link"))
	if err != nil || link != "data" {
		t.Fatal("symlink changed")
	}
	hard, err := os.Stat(filepath.Join(filepath.Dir(restored), "hardlink"))
	if err != nil || !os.SameFile(after, hard) {
		t.Fatal("hardlink changed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(restored), "skip.tmp")); !os.IsNotExist(err) {
		t.Fatal("excluded file restored")
	}
	if err := m.exec(ctx, nil, "check", "--read-data"); err != nil {
		t.Fatal(err)
	}
	// A wrong encryption password must fail.
	wrong := exec.Command("restic", "--no-cache", "snapshots")
	wrong.Env = append(os.Environ(), "RESTIC_REPOSITORY="+m.config.Repository.URL, "RESTIC_PASSWORD=incorrect")
	if err := wrong.Run(); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestIntegrationFailedDumpCreatesNoSnapshot(t *testing.T) {
	m := integrationManager(t)
	scriptDir, err := os.MkdirTemp("/tmp", "backup-manager-failing-dump-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scriptDir) })
	script := filepath.Join(scriptDir, "failing-pg-dump")
	marker := filepath.Join(scriptDir, "invoked")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'partial data'\ntouch '"+marker+"'\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	m.config.Postgres.Binary = script
	m.config.Postgres.Databases = []Database{{Name: "broken", Database: "broken"}}
	if err := m.backup(context.Background(), false); err == nil {
		t.Fatal("failed dump accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("dump command was not executed")
	}
	cmd := exec.Command("restic", "--no-cache", "snapshots", "--json")
	cmd.Env = m.env
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []any
	if err := json.Unmarshal(data, &snapshots); err != nil || len(snapshots) != 0 {
		t.Fatalf("failed dump left snapshot: %s %v", data, err)
	}
}

func TestIntegrationPostgresDumpRestore(t *testing.T) {
	m := integrationManager(t)
	if _, err := exec.LookPath("psql"); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bm_test_%d", time.Now().UnixNano())
	targetDB := name + "_restore"
	for _, db := range []string{name, targetDB} {
		runExternal(t, "createdb", db)
		t.Cleanup(func() { runExternal(t, "dropdb", "--force", db) })
	}
	runExternal(t, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-d", name, "-c",
		"CREATE TABLE backup_test(id integer PRIMARY KEY, value text); INSERT INTO backup_test VALUES (1,'hello'),(2,'world'); GRANT SELECT ON backup_test TO PUBLIC;")
	m.config.Postgres.Databases = []Database{{Name: "app", Database: name, Host: "/var/run/postgresql", User: "root"}}
	if err := m.backup(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "dump-restore")
	if err := m.restore(context.Background(), restoreOptions{Snapshot: "latest", Source: "db:app", Target: target}); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(target, "postgres-app.dump")
	runExternal(t, "pg_restore", "--exit-on-error", "--dbname", targetDB, dump)
	data := runExternal(t, "psql", "-XAt", "-d", targetDB, "-c", "SELECT string_agg(value, ',' ORDER BY id) FROM backup_test")
	if strings.TrimSpace(string(data)) != "hello,world" {
		t.Fatalf("bad restored rows: %s", data)
	}
	owner := runExternal(t, "psql", "-XAt", "-d", targetDB, "-c", "SELECT tableowner FROM pg_tables WHERE tablename='backup_test'")
	if strings.TrimSpace(string(owner)) != "root" {
		t.Fatal("table owner not restored")
	}
	grant := runExternal(t, "psql", "-XAt", "-d", targetDB, "-c", "SELECT count(*) FROM information_schema.table_privileges WHERE table_name='backup_test' AND grantee='PUBLIC' AND privilege_type='SELECT'")
	if strings.TrimSpace(string(grant)) != "1" {
		t.Fatal("table grants not restored")
	}
}
