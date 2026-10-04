package coraza

import (
	"github.com/corazawaf/coraza/v3/debuglog"
	"io"
)

// Unlike a disabled zap sink, this logger never builds fields or enables an
// event. CRS debug-level/output directives cannot reactivate it. The request
// remains solely in Coraza's native inspection transaction.
type privacyLogger struct{}

var _ debuglog.Logger = privacyLogger{}

func (privacyLogger) WithOutput(io.Writer) debuglog.Logger          { return privacyLogger{} }
func (privacyLogger) WithLevel(debuglog.Level) debuglog.Logger      { return privacyLogger{} }
func (privacyLogger) With(...debuglog.ContextField) debuglog.Logger { return privacyLogger{} }
func (privacyLogger) Trace() debuglog.Event                         { return noopEvent{} }
func (privacyLogger) Debug() debuglog.Event                         { return noopEvent{} }
func (privacyLogger) Info() debuglog.Event                          { return noopEvent{} }
func (privacyLogger) Warn() debuglog.Event                          { return noopEvent{} }
func (privacyLogger) Error() debuglog.Event                         { return noopEvent{} }
