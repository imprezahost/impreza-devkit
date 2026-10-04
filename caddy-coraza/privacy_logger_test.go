package coraza

import (
	"bytes"
	"github.com/corazawaf/coraza/v3/debuglog"
	"testing"
)

func TestPrivacyLoggerCannotCaptureFieldsOrBeReenabled(t *testing.T) {
	var output bytes.Buffer
	var logger debuglog.Logger = privacyLogger{}
	called := false
	logger = logger.WithOutput(&output).WithLevel(debuglog.LevelTrace).With(func(e debuglog.Event) debuglog.Event { called = true; return e.Str("visitor", "<VISITOR_ID>") })
	if called {
		t.Fatal("private logger evaluated visitor context fields")
	}
	for _, e := range []debuglog.Event{logger.Trace(), logger.Debug(), logger.Info(), logger.Warn(), logger.Error()} {
		if e.IsEnabled() {
			t.Fatal("private logger event enabled request formatting")
		}
		e.Str("visitor", "<VISITOR_ID>").Msg("<VISITOR_ID>")
	}
	if output.Len() != 0 {
		t.Fatal("private logger emitted visitor data")
	}
}
