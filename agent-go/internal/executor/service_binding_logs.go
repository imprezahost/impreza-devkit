package executor

import (
	"context"
	"fmt"
	"log/slog"
)

// Each operation receives a copied executor/logger; no global logger or
// concurrently running operation gains access to this operation's secrets.
type bindingLogHandler struct {
	inner  slog.Handler
	values map[string]string
}

func (h bindingLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}
func (h bindingLogHandler) attr(a slog.Attr) slog.Attr {
	a.Key = redactServiceBindingText(a.Key, h.values)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		copyAttrs := make([]slog.Attr, len(attrs))
		for i, v := range attrs {
			copyAttrs[i] = h.attr(v)
		}
		a.Value = slog.GroupValue(copyAttrs...)
	case slog.KindString:
		a.Value = slog.StringValue(redactServiceBindingText(v.String(), h.values))
	case slog.KindAny:
		text := fmt.Sprint(v.Any())
		clean := redactServiceBindingText(text, h.values)
		if clean != text {
			a.Value = slog.StringValue(clean)
		} else {
			a.Value = v
		}
	default:
		a.Value = v
	}
	return a
}
func (h bindingLogHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, redactServiceBindingText(r.Message, h.values), r.PC)
	r.Attrs(func(a slog.Attr) bool { clean.AddAttrs(h.attr(a)); return true })
	return h.inner.Handle(ctx, clean)
}
func (h bindingLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = h.attr(a)
	}
	return bindingLogHandler{inner: h.inner.WithAttrs(clean), values: h.values}
}
func (h bindingLogHandler) WithGroup(name string) slog.Handler {
	return bindingLogHandler{inner: h.inner.WithGroup(redactServiceBindingText(name, h.values)), values: h.values}
}
func (d *Docker) withServiceBindingLogRedaction(values map[string]string) *Docker {
	if len(values) == 0 || d.Log == nil {
		return d
	}
	clone := *d
	copyValues := make(map[string]string, len(values))
	for k, v := range values {
		copyValues[k] = v
	}
	clone.Log = slog.New(bindingLogHandler{inner: d.Log.Handler(), values: copyValues})
	return &clone
}
