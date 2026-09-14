package poll

import (
	"golang.org/x/sys/windows"
	"os"
)

func lockJournal(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// Production agents run on Linux; Windows supports local contract tests only.
func syncJournalDirectory(path string) error { return nil }
