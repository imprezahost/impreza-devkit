package caddyshield

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	caddy.RegisterModule(&ProxyMetricsMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("proxy_metrics", parseProxyMetricsCaddyfile)
}

// ProxyMetricsMiddleware counts requests through the proxy per
// deployment: status class, latency histogram, transferred bytes. Privacy
// contract: the ONLY label is the deployment id. No IP, no path, no hostname,
// no user agent — nothing that could identify a visitor is measured, stored
// or exported. Counters are scraped by the agent from the container-local
// admin endpoint and folded into the per-minute agent report.
type ProxyMetricsMiddleware struct {
	Deployment string `json:"deployment,omitempty"`
}

// Latency buckets in milliseconds, covering fast APIs to slow uploads.
var proxyLatencyBuckets = []float64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

var (
	proxyRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_proxy_requests_total",
		Help: "Proxy requests by deployment and HTTP status class. No visitor identity.",
	}, []string{"deployment", "class"})
	proxyLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "impreza_proxy_latency_ms",
		Help:    "Proxy request latency in milliseconds by deployment. No visitor identity.",
		Buckets: proxyLatencyBuckets,
	}, []string{"deployment"})
	proxyBytesIn = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_proxy_bytes_in_total",
		Help: "Request body bytes received by the proxy, by deployment.",
	}, []string{"deployment"})
	proxyBytesOut = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_proxy_bytes_out_total",
		Help: "Response body bytes written by the proxy, by deployment.",
	}, []string{"deployment"})
)

func init() {
	prometheus.MustRegister(proxyRequests, proxyLatency, proxyBytesIn, proxyBytesOut)
}

// CaddyModule returns the Caddy module information.
func (*ProxyMetricsMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.proxy_metrics",
		New: func() caddy.Module { return &ProxyMetricsMiddleware{} },
	}
}

func (m *ProxyMetricsMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "deployment":
				if !d.AllArgs(&m.Deployment) {
					return d.ArgErr()
				}
			default:
				return d.Errf("unknown subdirective %q", d.Val())
			}
		}
	}
	return nil
}

func parseProxyMetricsCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m ProxyMetricsMiddleware
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

func (m *ProxyMetricsMiddleware) Provision(ctx caddy.Context) error {
	if m.Deployment == "" {
		return fmt.Errorf("proxy_metrics: deployment is required")
	}
		registerMetrics(ctx)
	return nil
}

func (m *ProxyMetricsMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	start := time.Now()
	rec := &countingWriter{ResponseWriter: w}
	err := next.ServeHTTP(rec, r)
	elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0

	class := statusClass(rec.status)
	if err != nil && rec.status == 0 {
		class = "5xx" // handler error before any write
	}
	proxyRequests.WithLabelValues(m.Deployment, class).Inc()
	proxyLatency.WithLabelValues(m.Deployment).Observe(elapsedMs)
	if r.ContentLength > 0 {
		proxyBytesIn.WithLabelValues(m.Deployment).Add(float64(r.ContentLength))
	}
	proxyBytesOut.WithLabelValues(m.Deployment).Add(float64(rec.written))
	return err
}

// statusClass maps a status code to its 1xx–5xx class; 0 counts as 5xx so a
// handler crash is visible in the error rate.
func statusClass(code int) string {
	switch {
	case code >= 100 && code < 200:
		return "1xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// countingWriter records the first status written and the body byte count.
type countingWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.written += int64(n)
	return n, err
}

// Flush propagates so SSE/streaming responses are not buffered by the wrap.
func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// FormatCounterName builds the exposition name the agent scraper looks for.
func FormatCounterName(metric string) string { return "impreza_proxy_" + metric }

// ParseExpositionValue is kept for symmetry with the agent-side scraper; the
// agent parses the text format itself.
func ParseExpositionValue(line string) (float64, bool) {
	for i := len(line) - 1; i >= 0; i-- {
		if line[i] == ' ' {
			v, err := strconv.ParseFloat(line[i+1:], 64)
			return v, err == nil
		}
	}
	return 0, false
}

// Interface guard.
var _ caddyhttp.MiddlewareHandler = (*ProxyMetricsMiddleware)(nil)
