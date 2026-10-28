package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const helpText = `backup-manager — encrypted Linux server backups using restic

Usage: backup-manager [--config /etc/backup-manager/config.yaml] COMMAND [OPTIONS]

Commands:
  help         Show this help (COMMAND --help shows command options)
  version      Show version
  validate     Validate YAML, permissions and settings (offline)
  init         Initialize the configured encrypted repository (once)
  backup       Back up files and PostgreSQL; --dry-run previews (still runs pg_dump)
  snapshots    List snapshots for this host/job; --json for machine-readable output
  ls           List snapshot contents; --source files|db:NAME --snapshot latest|ID
  restore      Restore and verify files/DB dump; --target ABSOLUTE_DIRECTORY required
  check        Check repository integrity; --read-data verifies all data
  forget       Apply configured retention; --dry-run previews; --prune frees space

Examples:
  backup-manager --config /etc/backup-manager/config.yaml backup
  backup-manager restore --source files --snapshot latest --target /srv/recovery
  backup-manager restore --source db:app --target /srv/recovery-db

Repository commands require Linux. Restore as root to preserve UID/GID and metadata.
PostgreSQL restore extracts a custom-format dump; import separately with pg_restore.
`

type stringList []string

func (s *stringList) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runCLI(ctx context.Context, args []string, out, errOut io.Writer) error {
	root := flag.NewFlagSet("backup-manager", flag.ContinueOnError)
	root.SetOutput(errOut)
	configPath := root.String("config", "/etc/backup-manager/config.yaml", "configuration YAML (before command)")
	root.Usage = func() { fmt.Fprint(out, helpText) }
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	remaining := root.Args()
	if len(remaining) == 0 {
		fmt.Fprint(out, helpText)
		return nil
	}
	name := remaining[0]
	if name == "help" || name == "version" {
		if len(remaining) > 1 {
			return errors.New("help/version accept no arguments; use COMMAND --help")
		}
		if name == "help" {
			fmt.Fprint(out, helpText)
		} else {
			fmt.Fprintln(out, version)
		}
		return nil
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	var dry, json, readData, prune bool
	o := restoreOptions{Snapshot: "latest", Source: "files"}
	var include stringList
	switch name {
	case "validate", "init":
	case "backup":
		fs.BoolVar(&dry, "dry-run", false, "preview without saving snapshots (pg_dump still runs)")
	case "snapshots":
		fs.BoolVar(&json, "json", false, "output JSON")
	case "ls", "restore":
		fs.StringVar(&o.Snapshot, "snapshot", "latest", "latest or hexadecimal snapshot ID")
		fs.StringVar(&o.Source, "source", "files", "files or db:NAME (also accepts old, removed database names)")
		if name == "restore" {
			fs.StringVar(&o.Target, "target", "", "absolute destination directory (required)")
			fs.BoolVar(&o.Overwrite, "overwrite", false, "allow restoring into a nonempty directory")
			fs.BoolVar(&o.AllowRoot, "allow-root", false, "allow restoring file paths directly to / (requires --overwrite)")
			fs.Var(&include, "include", "restic include pattern; repeatable")
		}
	case "check":
		fs.BoolVar(&readData, "read-data", false, "download and verify ALL repository data")
	case "forget":
		fs.BoolVar(&dry, "dry-run", false, "show retention decisions without deleting snapshots")
		fs.BoolVar(&prune, "prune", false, "also remove unused data from the repository")
	default:
		return fmt.Errorf("unknown command %q; use help", name)
	}
	if err := fs.Parse(remaining[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("unexpected positional arguments; use COMMAND --help")
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if name == "validate" {
		fmt.Fprintln(out, "Configuration OK")
		return nil
	}
	unlock, err := acquireLock(c.LockFile)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := newManager(c, out, errOut)
	if err != nil {
		return err
	}
	switch name {
	case "init":
		return m.exec(ctx, nil, "init", "--repository-version", "2")
	case "backup":
		return m.backup(ctx, dry)
	case "snapshots":
		a := []string{"snapshots", "--host", c.Host, "--tag", m.tag()}
		if json {
			a = append(a, "--json")
		}
		return m.exec(ctx, nil, a...)
	case "ls":
		if o.Snapshot != "latest" && !snapshotID.MatchString(o.Snapshot) {
			return errors.New("snapshot must be latest or a hexadecimal snapshot ID")
		}
		a, err := m.snapshotSelection(ctx, o.Snapshot, o.Source)
		if err != nil {
			return err
		}
		return m.exec(ctx, nil, append([]string{"ls", o.Snapshot}, a...)...)
	case "restore":
		o.Include = include
		return m.restore(ctx, o)
	case "check":
		a := []string{"check"}
		if readData {
			a = append(a, "--read-data")
		}
		return m.exec(ctx, nil, a...)
	case "forget":
		return m.forget(ctx, dry, prune)
	}
	return nil
}
