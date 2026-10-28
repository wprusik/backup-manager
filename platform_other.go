//go:build !linux

package main

import (
	"errors"
	"os"
	"os/exec"
)

func configureCommand(*exec.Cmd) {}

func checkOwner(os.FileInfo) error { return nil }
func checkTmpfs(string) error      { return errors.New("tmpfs verification requires Linux") }
func acquireLock(string) (func(), error) {
	return nil, errors.New("repository commands require Linux; help, version and validate work on other platforms")
}
