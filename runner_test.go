package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunnerHelper(t *testing.T) {
	if os.Getenv("BACKUP_MANAGER_HELPER") == "" {
		return
	}
	if os.Getenv("BACKUP_MANAGER_HELPER") == "wait" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("secret-in-output")
	_, _ = os.Stderr.WriteString("secret-in-output")
	os.Exit(3)
}

func TestExecRunnerPropagatesExitCodeAndRedactsBothStreams(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	r := execRunner{Out: &out, Err: &errOut, Secrets: []string{"secret-in-output"}}
	err = r.Run(context.Background(), command{Binary: executable, Args: []string{"-test.run=^TestRunnerHelper$"},
		Env: append(os.Environ(), "BACKUP_MANAGER_HELPER=exit", "GOCOVERDIR="+t.TempDir()), Label: "helper"})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("failure lost: %v", err)
	}
	if out.String() != "[REDACTED]" || errOut.String() != "[REDACTED]" {
		t.Fatalf("secret exposed: %q %q", out.String(), errOut.String())
	}
}

func TestExecRunnerCancellation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out, errOut bytes.Buffer
	r := execRunner{Out: &out, Err: &errOut}
	start := time.Now()
	err = r.Run(ctx, command{Binary: executable, Args: []string{"-test.run=^TestRunnerHelper$"},
		Env: append(os.Environ(), "BACKUP_MANAGER_HELPER=wait", "GOCOVERDIR="+t.TempDir()), Label: "helper"})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("cancellation did not stop child: %v", err)
	}
}
