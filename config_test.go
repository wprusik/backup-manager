package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	c := defaultConfig()
	c.Host = "test-server"
	c.Repository = Repository{URL: filepath.Join(dir, "repo"), Password: "repository-secret-123"}
	c.LockFile = filepath.Join(dir, "lock")
	c.Restic.TempDir = filepath.Join(dir, "temp")
	c.Restic.RequireTmpfs = false
	return c
}

func writeConfig(t *testing.T, c Config) string {
	t.Helper()
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return writeYAML(t, string(data))
}

func writeYAML(t *testing.T, data string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestConfigStrictAndSecretsNotDisclosed(t *testing.T) {
	c := testConfig(t)
	filename := writeConfig(t, c)
	loaded, err := loadConfig(filename)
	if err != nil || loaded.Repository.Password != c.Repository.Password {
		t.Fatalf("load: %v", err)
	}
	data, _ := os.ReadFile(filename)
	for _, suffix := range []string{"\nunknown: secret-marker", "\nid: secret-marker", "\n---\nid: extra"} {
		_, err := loadConfig(writeYAML(t, string(data)+suffix))
		if err == nil || strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("invalid YAML error: %v", err)
		}
	}
	_, err = loadConfig(writeYAML(t, "restic:\n  connections: super-secret-value\n"))
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("scalar leaked: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"relative source":    func(c *Config) { c.Files.Paths = []string{"relative"} },
		"ambiguous password": func(c *Config) { c.Repository.PasswordFile = filepath.Join(t.TempDir(), "pass") },
		"missing password":   func(c *Config) { c.Repository.Password = "" },
		"duplicate database": func(c *Config) {
			c.Postgres.Databases = []Database{{Name: "app", Database: "app"}, {Name: "app", Database: "other"}}
		},
		"database connstring": func(c *Config) {
			c.Postgres.Databases = []Database{{Name: "app", Database: "host=evil password=secret"}}
		},
		"resource bound":     func(c *Config) { c.Restic.Connections = 100 },
		"negative retention": func(c *Config) { c.Retention.Daily = -1 },
		"comma tag":          func(c *Config) { c.ID = "id,other" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			mutate(&c)
			if err := c.validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConfigPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	p := writeConfig(t, testConfig(t))
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(p); err == nil {
		t.Fatal("accepted world-readable credentials")
	}
}

func TestCLIHelpAndOfflineValidation(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"version"}, {"restore", "--help"}} {
		var out bytes.Buffer
		if err := runCLI(context.Background(), args, &out, &out); err != nil || out.Len() == 0 {
			t.Fatalf("%v: %v", args, err)
		}
	}
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"--config", writeConfig(t, testConfig(t)), "validate"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"oops"}, {"backup", "extra"}, {"backup", "--typo"}} {
		if err := runCLI(context.Background(), args, &out, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
