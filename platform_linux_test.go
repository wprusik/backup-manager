//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLockExcludesOverlappingOperationsAndCanBeReacquired(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manager.lock")
	unlock, err := acquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := acquireLock(p); err == nil {
		other()
		t.Fatal("overlap allowed")
	}
	unlock()
	unlock, err = acquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestRestoreRootSymlinkGuard(t *testing.T) {
	m, _ := fakeManager(t, testConfig(t))
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	o := restoreOptions{Snapshot: "latest", Source: "files", Target: link}
	if err := m.restore(context.Background(), o); err == nil {
		t.Fatal("root symlink bypassed guard")
	}
}
