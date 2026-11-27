package cmd

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type confirmationFaultReader struct{}

func (confirmationFaultReader) Read(buffer []byte) (int, error) {
	return copy(buffer, "yes"), io.ErrUnexpectedEOF
}

type confirmationFaultWriter struct{}

func (confirmationFaultWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestDestructiveConfirmationRequiresWorkingIO(t *testing.T) {
	if err := confirmOrExit(confirmationFaultReader{}, io.Discard, "fixture change", false); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("partial_read: expected the input error, got %v", err)
	}
	if err := confirmOrExit(strings.NewReader("yes\n"), confirmationFaultWriter{}, "fixture change", false); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("prompt_write: expected the output error, got %v", err)
	}
	if err := confirmOrExit(strings.NewReader("yes"), io.Discard, "fixture change", false); err != nil {
		t.Errorf("explicit_eof: an explicit answer at EOF should still work: %v", err)
	}
}
