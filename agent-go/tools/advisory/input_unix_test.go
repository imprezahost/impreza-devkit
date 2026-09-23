//go:build !windows

package main

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateInputBoundaries(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	put(t, key, []byte("fixture"))
	if _, e := readRegularFile(key, 128, true); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(key, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := readRegularFile(key, 128, true); e == nil {
		t.Fatal("public seed accepted")
	}
	link := filepath.Join(dir, "link")
	if e := os.Symlink(key, link); e != nil {
		t.Fatal(e)
	}
	if _, e := boundedFile(link, 128); e == nil {
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if e := unix.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := boundedFile(fifo, 128); done <- e }()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("fifo accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("fifo blocked input")
	}
}
