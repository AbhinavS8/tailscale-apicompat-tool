package main

import (
	"errors"
	"flag"
	"fmt"
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
