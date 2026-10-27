package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"golang.org/x/mod/module"
)

type usageError struct {
	err error
}

func (e *usageError) Error() string {
	msg := ""
	if !errors.Is(e.err, flag.ErrHelp) {
		msg = e.err.Error()
	}
	return "usage: apicompat [-base=version (optional)]\n" + msg
}

// simplify returning usageError
func usageErrorf(format string, args ...interface{}) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

type baseVersionError struct {
	err     error
	modPath string
}

func (e *baseVersionError) Error() string {
	firstVersion := "v0.1.0"
	_, major, _ := module.SplitPathVersion(e.modPath)
	if major != "" {
		firstVersion = major[1:] + ".0.0"
	}

	return fmt.Sprintf("could not find base version. Consider setting -version=%s if this is a first release, or explicitly set -base=none: %v", firstVersion, e.err)
}

func (e *baseVersionError) Unwrap() error {
	return e.err
}

type downloadError struct {
	m   module.Version
	err error
}

func (e *downloadError) Error() string {
	msg := e.err.Error()
	sep := " "
	if strings.Contains(msg, "\n") {
		sep = "\n"
	}
	return fmt.Sprintf("error downloading module %s@%s:%s%s", e.m.Path, e.m.Version, sep, msg)
}
