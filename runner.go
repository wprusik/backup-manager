package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type command struct {
	Binary string
	Args   []string
	Env    []string
	Label  string
	Output io.Writer // Optional bounded metadata capture, never used for backup data.
}

type runner interface {
	Run(context.Context, command) error
}

type execRunner struct {
	Out, Err io.Writer
	Secrets  []string
}

func (r execRunner) Run(ctx context.Context, c command) error {
	cmd := exec.CommandContext(ctx, c.Binary, c.Args...)
	cmd.Env = c.Env
	cmd.WaitDelay = 5 * time.Second
	configureCommand(cmd)
	dst := r.Out
	if c.Output != nil {
		dst = c.Output
	}
	out := &redactor{dst: dst, secrets: r.Secrets}
	errOut := &redactor{dst: r.Err, secrets: r.Secrets}
	cmd.Stdout, cmd.Stderr = out, errOut
	err := cmd.Run()
	flushErr := out.flush()
	flushErr2 := errOut.flush()
	if err != nil {
		return fmt.Errorf("%s failed: %w", c.Label, err)
	}
	if flushErr != nil {
		return flushErr
	}
	return flushErr2
}

// Keep only the possible secret suffix between writes, never an entire log or dump.
type redactor struct {
	dst     io.Writer
	secrets []string
	pending []byte
}

func (w *redactor) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	keep := 0
	for _, s := range w.secrets {
		if len(s) > keep {
			keep = len(s)
		}
	}
	if keep > 0 {
		keep--
	}
	for len(w.pending) > keep {
		cut := len(w.pending) - keep
		start, length := len(w.pending), 0
		for _, s := range w.secrets {
			if s == "" {
				continue
			}
			if i := bytes.Index(w.pending, []byte(s)); i >= 0 && (i < start || i == start && len(s) > length) {
				start, length = i, len(s)
			}
		}
		if start < cut {
			if _, err := w.dst.Write(w.pending[:start]); err != nil {
				return 0, err
			}
			if _, err := io.WriteString(w.dst, "[REDACTED]"); err != nil {
				return 0, err
			}
			w.pending = w.pending[start+length:]
		} else {
			if _, err := w.dst.Write(w.pending[:cut]); err != nil {
				return 0, err
			}
			w.pending = w.pending[cut:]
		}
	}
	return len(p), nil
}

func (w *redactor) flush() error {
	s := string(w.pending)
	for _, secret := range w.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	w.pending = nil
	_, err := io.WriteString(w.dst, s)
	return err
}
