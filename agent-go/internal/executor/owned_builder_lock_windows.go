package executor

import (
	"errors"
	"os"
)

// Production execution is Linux-only. Do not pretend Windows filesystem semantics
// certify durable ownership; portable identity tests still run there.
func openOwnedBuilderLock(string) (*os.File, error) {
	return nil, errors.New("owned build execution requires Linux")
}
func lockOwnedBuilderFile(*os.File) error { return errors.New("owned build execution requires Linux") }

func openOwnedBuilderCID(string) (*os.File, error) {
	return nil, errors.New("owned build execution requires Linux")
}
