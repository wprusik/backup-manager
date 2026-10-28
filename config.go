package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ID         string     `yaml:"id"`
	Host       string     `yaml:"host"`
	LockFile   string     `yaml:"lock_file"`
	Repository Repository `yaml:"repository"`
	Restic     Restic     `yaml:"restic"`
	Files      Files      `yaml:"files"`
	Postgres   Postgres   `yaml:"postgres"`
	Retention  Retention  `yaml:"retention"`
}

type Repository struct {
	URL          string `yaml:"url"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	S3           S3     `yaml:"s3"`
}

type S3 struct {
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	Region          string `yaml:"region"`
}

type Restic struct {
	Binary          string `yaml:"binary"`
	TempDir         string `yaml:"temp_dir"`
	RequireTmpfs    bool   `yaml:"require_tmpfs"`
	PackSizeMiB     int    `yaml:"pack_size_mib"`
	Connections     int    `yaml:"connections"`
	ReadConcurrency int    `yaml:"read_concurrency"`
	MaxProcs        int    `yaml:"max_procs"`
	MemoryLimit     string `yaml:"memory_limit"`
}

type Files struct {
	Paths         []string `yaml:"paths"`
	Exclude       []string `yaml:"exclude"`
	OneFileSystem bool     `yaml:"one_file_system"`
	WithAtime     bool     `yaml:"with_atime"`
}

type Postgres struct {
	Binary    string     `yaml:"binary"`
	Databases []Database `yaml:"databases"`
}

type Database struct {
	Name           string `yaml:"name"`
	Database       string `yaml:"database"`
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	User           string `yaml:"user"`
	Password       string `yaml:"password"`
	SSLMode        string `yaml:"sslmode"`
	ConnectTimeout int    `yaml:"connect_timeout"`
}

type Retention struct {
	Daily   int `yaml:"daily"`
	Weekly  int `yaml:"weekly"`
	Monthly int `yaml:"monthly"`
	Yearly  int `yaml:"yearly"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var memorySize = regexp.MustCompile(`^[1-9][0-9]*(MiB|GiB)$`)

func defaultConfig() Config {
	host, _ := os.Hostname()
	return Config{
		ID: "server", Host: host, LockFile: "/run/backup-manager/manager.lock",
		Restic: Restic{Binary: "restic", TempDir: "/run/backup-manager-tmp", RequireTmpfs: true,
			PackSizeMiB: 16, Connections: 2, ReadConcurrency: 1, MaxProcs: 2, MemoryLimit: "512MiB"},
		Postgres: Postgres{Binary: "pg_dump"},
	}
}

func loadConfig(filename string) (Config, error) {
	c := defaultConfig()
	f, err := os.Open(filename)
	if err != nil {
		return c, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return c, errors.New("configuration must be a regular file accessible only to its owner (chmod 600)")
	}
	if err := checkOwner(info); err != nil {
		return c, err
	}
	if info.Size() > 1<<20 {
		return c, errors.New("configuration exceeds 1 MiB")
	}
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		// Parser errors may contain secret scalar values.
		return c, errors.New("invalid YAML configuration (check types, duplicate keys and unknown fields)")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	return c, c.validate()
}

func (c Config) backend() string {
	if strings.HasPrefix(c.Repository.URL, "sftp:") {
		return "sftp"
	}
	if strings.HasPrefix(c.Repository.URL, "s3:") {
		return "s3"
	}
	return "local"
}

func (c Config) validate() error {
	if !identifier.MatchString(c.ID) || c.Host == "" || strings.ContainsAny(c.Host, ",\r\n") {
		return errors.New("id must be a simple identifier; host must be nonempty and contain no comma/newline")
	}
	r := c.Repository
	if r.URL == "" || strings.ContainsAny(r.URL, "\r\n") {
		return errors.New("repository.url is required and must not contain newlines")
	}
	if c.backend() == "local" && !filepath.IsAbs(r.URL) {
		return errors.New("repository.url must be an absolute local path, sftp: URL or s3: URL")
	}
	if c.backend() == "s3" || strings.HasPrefix(r.URL, "sftp://") {
		value := r.URL
		if c.backend() == "s3" {
			value = strings.TrimPrefix(value, "s3:")
		}
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid repository URL (host required, queries/fragments unsupported)")
		}
		if u.User != nil {
			if _, ok := u.User.Password(); ok || c.backend() == "s3" {
				return errors.New("do not embed passwords or S3 credentials in repository URLs")
			}
		}
	}
	if (r.Password == "") == (r.PasswordFile == "") {
		return errors.New("set exactly one of repository.password and repository.password_file")
	}
	if r.PasswordFile != "" && !filepath.IsAbs(r.PasswordFile) {
		return errors.New("repository.password_file must be absolute")
	}
	if (r.S3.AccessKeyID == "") != (r.S3.SecretAccessKey == "") {
		return errors.New("set both S3 access_key_id and secret_access_key")
	}
	if c.backend() == "s3" && r.S3.AccessKeyID == "" {
		return errors.New("S3 requires access_key_id and secret_access_key in YAML")
	}
	if !filepath.IsAbs(c.LockFile) || !filepath.IsAbs(c.Restic.TempDir) {
		return errors.New("lock_file and restic.temp_dir must be absolute paths")
	}
	t := c.Restic
	if t.Binary == "" || c.Postgres.Binary == "" || t.PackSizeMiB < 4 || t.PackSizeMiB > 128 ||
		t.Connections < 1 || t.Connections > 16 || t.ReadConcurrency < 1 || t.ReadConcurrency > 16 ||
		t.MaxProcs < 1 || t.MaxProcs > 64 || !memorySize.MatchString(t.MemoryLimit) {
		return errors.New("invalid restic resources: pack 4..128 MiB, connections/read_concurrency 1..16, max_procs 1..64, memory_limit e.g. 512MiB")
	}
	for _, p := range c.Files.Paths {
		if !filepath.IsAbs(p) {
			return errors.New("all files.paths must be absolute")
		}
	}
	seen := make(map[string]bool)
	for _, db := range c.Postgres.Databases {
		if !identifier.MatchString(db.Name) || seen[db.Name] {
			return errors.New("database names must be unique simple identifiers")
		}
		seen[db.Name] = true
		if db.Database == "" || strings.ContainsAny(db.Database, "=\r\n") || strings.Contains(db.Database, "://") {
			return errors.New("postgres database must be a database name, not a connection string")
		}
		if db.Port < 0 || db.Port > 65535 || db.ConnectTimeout < 0 || db.ConnectTimeout > 3600 {
			return errors.New("invalid PostgreSQL port or connect_timeout")
		}
		switch db.SSLMode {
		case "", "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		default:
			return errors.New("invalid postgres sslmode")
		}
	}
	for _, n := range []int{c.Retention.Daily, c.Retention.Weekly, c.Retention.Monthly, c.Retention.Yearly} {
		if n < 0 {
			return errors.New("retention values cannot be negative")
		}
	}
	return nil
}
