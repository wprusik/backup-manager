package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

type manager struct {
	config Config
	runner runner
	env    []string
	out    io.Writer
}

func newManager(c Config, out, errOut io.Writer) (*manager, error) {
	password := c.Repository.Password
	if c.Repository.PasswordFile != "" {
		f, err := os.Open(c.Repository.PasswordFile)
		if err != nil {
			return nil, errors.New("cannot open repository password_file")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return nil, errors.New("password_file must be a regular file <= 1 MiB with mode 600")
		}
		if err := checkOwner(info); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, 1<<20))
		if err != nil {
			return nil, err
		}
		password = strings.TrimRight(string(data), "\r\n")
		if password == "" {
			return nil, errors.New("empty repository password_file")
		}
	}
	if err := os.MkdirAll(c.Restic.TempDir, 0700); err != nil {
		return nil, fmt.Errorf("prepare temp_dir: %w", err)
	}
	if c.Restic.RequireTmpfs {
		if err := checkTmpfs(c.Restic.TempDir); err != nil {
			return nil, err
		}
	}
	// Never inherit restic/PG settings that could silently redirect a backup or write a dump to disk.
	var env []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		u := strings.ToUpper(key)
		if strings.HasPrefix(u, "RESTIC_") || strings.HasPrefix(u, "AWS_") || strings.HasPrefix(u, "PG") ||
			u == "TMPDIR" || u == "TMP" || u == "TEMP" || u == "GOMEMLIMIT" || u == "GOMAXPROCS" {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "RESTIC_REPOSITORY="+c.Repository.URL, "RESTIC_PASSWORD="+password,
		"TMPDIR="+c.Restic.TempDir, "TMP="+c.Restic.TempDir, "TEMP="+c.Restic.TempDir,
		"GOMEMLIMIT="+c.Restic.MemoryLimit, "GOMAXPROCS="+strconv.Itoa(c.Restic.MaxProcs))
	secrets := []string{password, c.Repository.S3.AccessKeyID, c.Repository.S3.SecretAccessKey}
	if c.backend() == "s3" {
		env = append(env, "AWS_ACCESS_KEY_ID="+c.Repository.S3.AccessKeyID,
			"AWS_SECRET_ACCESS_KEY="+c.Repository.S3.SecretAccessKey)
	}
	for _, db := range c.Postgres.Databases {
		secrets = append(secrets, db.Password)
	}
	return &manager{config: c, env: env, runner: execRunner{Out: out, Err: errOut, Secrets: secrets}, out: out}, nil
}

func (m *manager) exec(ctx context.Context, extraEnv []string, args ...string) error {
	return m.execOutput(ctx, extraEnv, nil, args...)
}

func (m *manager) execOutput(ctx context.Context, extraEnv []string, output io.Writer, args ...string) error {
	c := m.config
	global := []string{"--no-cache", "--pack-size", strconv.Itoa(c.Restic.PackSizeMiB),
		"-o", c.backend() + ".connections=" + strconv.Itoa(c.Restic.Connections)}
	if c.backend() == "s3" && c.Repository.S3.Region != "" {
		global = append(global, "-o", "s3.region="+c.Repository.S3.Region)
	}
	return m.runner.Run(ctx, command{Binary: c.Restic.Binary, Args: append(global, args...),
		Env: append(append([]string{}, m.env...), extraEnv...), Label: "restic " + args[0], Output: output})
}

func (m *manager) tag() string { return "backup-manager:" + m.config.ID }

func (m *manager) selection(source string) ([]string, error) {
	tag := m.tag()
	if source == "files" {
		tag += ",source:files"
	} else if strings.HasPrefix(source, "db:") && identifier.MatchString(strings.TrimPrefix(source, "db:")) {
		tag += ",source:" + source
	} else {
		return nil, errors.New("source must be files or db:<name>")
	}
	return []string{"--host", m.config.Host, "--tag", tag}, nil
}

// Restic applies host/tag filters only to "latest", so validate explicit IDs ourselves.
func (m *manager) snapshotSelection(ctx context.Context, snapshot, source string) ([]string, error) {
	if snapshot != "latest" && !snapshotID.MatchString(snapshot) {
		return nil, errors.New("snapshot must be latest or a hexadecimal snapshot ID")
	}
	args, err := m.selection(source)
	if err != nil || snapshot == "latest" {
		return args, err
	}
	var data cappedBuffer
	if err := m.execOutput(ctx, nil, &data, "cat", "snapshot", snapshot); err != nil {
		return nil, err
	}
	var metadata struct {
		Host string   `json:"hostname"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data.Bytes(), &metadata); err != nil {
		return nil, errors.New("invalid snapshot metadata")
	}
	if metadata.Host != m.config.Host || !slices.Contains(metadata.Tags, m.tag()) || !slices.Contains(metadata.Tags, "source:"+source) {
		return nil, errors.New("snapshot does not match configured host, job and requested source")
	}
	return args, nil
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("snapshot metadata exceeds 1 MiB")
	}
	return b.Buffer.Write(p)
}

func (m *manager) backup(ctx context.Context, dry bool) error {
	c := m.config
	if len(c.Files.Paths) == 0 && len(c.Postgres.Databases) == 0 {
		return errors.New("no backup sources configured")
	}
	// A missing explicitly requested path must never look like a successful backup.
	for _, p := range c.Files.Paths {
		if _, err := os.Lstat(p); err != nil {
			return fmt.Errorf("backup source unavailable: %w", err)
		}
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	runTag := "run:" + hex.EncodeToString(id)
	fmt.Fprintln(m.out, "Backup", runTag)
	base := func(source string) []string {
		a := []string{"backup", "--host", c.Host, "--tag", m.tag(), "--tag", "source:" + source,
			"--tag", runTag, "--read-concurrency", strconv.Itoa(c.Restic.ReadConcurrency)}
		if dry {
			a = append(a, "--dry-run")
		}
		return a
	}
	if len(c.Files.Paths) > 0 {
		a := base("files")
		if c.Files.OneFileSystem {
			a = append(a, "--one-file-system")
		}
		if c.Files.WithAtime {
			a = append(a, "--with-atime")
		}
		for _, p := range append(append([]string{}, c.Files.Exclude...), c.Restic.TempDir, c.LockFile) {
			a = append(a, "--exclude", p)
		}
		if c.backend() == "local" {
			a = append(a, "--exclude", c.Repository.URL)
		}
		a = append(a, "--")
		a = append(a, c.Files.Paths...)
		if err := m.exec(ctx, nil, a...); err != nil {
			return err
		}
	}
	for _, db := range c.Postgres.Databases {
		a := base("db:" + db.Name)
		a = append(a, "--stdin-from-command", "--stdin-filename", "postgres-"+db.Name+".dump", "--",
			c.Postgres.Binary, "--no-password", "--format=custom", "--compress=0")
		env := []string{"PGDATABASE=" + db.Database, "PGPASSWORD=" + db.Password}
		if db.Host != "" {
			env = append(env, "PGHOST="+db.Host)
		}
		if db.User != "" {
			env = append(env, "PGUSER="+db.User)
		}
		if db.Port != 0 {
			env = append(env, "PGPORT="+strconv.Itoa(db.Port))
		}
		if db.SSLMode != "" {
			env = append(env, "PGSSLMODE="+db.SSLMode)
		}
		timeout := db.ConnectTimeout
		if timeout == 0 {
			timeout = 15
		}
		env = append(env, "PGCONNECT_TIMEOUT="+strconv.Itoa(timeout))
		if err := m.exec(ctx, env, a...); err != nil {
			return fmt.Errorf("database %s: %w", db.Name, err)
		}
	}
	fmt.Fprintln(m.out, "Backup finished:", runTag)
	return nil
}

type restoreOptions struct {
	Snapshot, Source, Target string
	Include                  []string
	AllowRoot, Overwrite     bool
}

var snapshotID = regexp.MustCompile(`^[a-fA-F0-9]{8,64}$`)

func (m *manager) restore(ctx context.Context, o restoreOptions) error {
	if o.Snapshot != "latest" && !snapshotID.MatchString(o.Snapshot) {
		return errors.New("snapshot must be latest or a hexadecimal snapshot ID")
	}
	if !filepath.IsAbs(o.Target) {
		return errors.New("restore requires an absolute --target")
	}
	target := filepath.Clean(o.Target)
	// Resolve existing symlink ancestors before deciding whether the target is /.
	probe := target
	for {
		if _, err := os.Lstat(probe); err == nil {
			real, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(probe, target)
			if err != nil {
				return err
			}
			target = filepath.Clean(filepath.Join(real, rel))
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return errors.New("cannot resolve restore target")
		}
		probe = parent
	}
	root := filepath.Dir(target) == target
	if root && (!o.AllowRoot || !o.Overwrite || o.Source != "files") {
		return errors.New("restoring to / requires --allow-root --overwrite --source files")
	}
	if !o.Overwrite {
		entries, err := os.ReadDir(target)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(entries) > 0 {
			return errors.New("restore target is not empty; use a new directory or explicit --overwrite")
		}
	}
	selectArgs, err := m.snapshotSelection(ctx, o.Snapshot, o.Source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if strings.HasPrefix(o.Source, "db:") {
		if err := os.Chmod(target, 0700); err != nil {
			return err
		}
	}
	a := append([]string{"restore", o.Snapshot, "--target", target, "--verify"}, selectArgs...)
	for _, p := range o.Include {
		a = append(a, "--include", p)
	}
	return m.exec(ctx, nil, a...)
}

func (m *manager) forget(ctx context.Context, dry, prune bool) error {
	r := m.config.Retention
	if r.Daily+r.Weekly+r.Monthly+r.Yearly == 0 {
		return errors.New("refusing forget without a nonzero retention policy")
	}
	a := []string{"forget", "--host", m.config.Host, "--tag", m.tag(), "--group-by", "host,paths",
		"--keep-daily", strconv.Itoa(r.Daily), "--keep-weekly", strconv.Itoa(r.Weekly),
		"--keep-monthly", strconv.Itoa(r.Monthly), "--keep-yearly", strconv.Itoa(r.Yearly)}
	if dry {
		a = append(a, "--dry-run")
	}
	if prune {
		a = append(a, "--prune")
	}
	return m.exec(ctx, nil, a...)
}
