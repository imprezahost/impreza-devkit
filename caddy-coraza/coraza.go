// Copyright 2025 The OWASP Coraza contributors
// SPDX-License-Identifier: Apache-2.0

package coraza

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	coreruleset "github.com/corazawaf/coraza-coreruleset/v4"
	"github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/types"
	shield "github.com/imprezahost/impreza-devkit/caddy-shield"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/jcchavezs/mergefs"
	mergefsio "github.com/jcchavezs/mergefs/io"
	"go.uber.org/zap"
)

// wafPool is a process-global pool that allows WAF instances to be shared
// across Caddy config reloads. When two consecutive configs use the same
// WAF configuration, the pool returns the existing WAF instead of building
// a new one, saving both memory and CPU.
var wafPool = caddy.NewUsagePool()

func init() {
	caddy.RegisterModule(corazaModule{})
	httpcaddyfile.RegisterHandlerDirective("coraza_waf", parseCaddyfile)
}

// pooledWAF wraps a coraza.WAF so it can be stored in a caddy.UsagePool.
// It implements caddy.Destructor so the pool can clean it up when all
// references are released.
type pooledWAF struct {
	waf coraza.WAF
}

func (p *pooledWAF) Destruct() error {
	var err error
	if c, ok := p.waf.(io.Closer); ok {
		if cerr := c.Close(); cerr != nil {
			err = fmt.Errorf("closing WAF: %w", cerr)
		}
	}
	p.waf = nil
	return err
}

// corazaModule is a Web Application Firewall implementation for Caddy.
type corazaModule struct {
	// deprecated
	Include []string `json:"include"`

	Directives    string `json:"directives"`
	LoadOWASPCRS  bool   `json:"load_owasp_crs"`
	TxIDReqHeader string `json:"tx_id_req_header"`
	Deployment    string `json:"deployment,omitempty"`
	ShieldMode    string `json:"shield_mode,omitempty"`

	logger  *zap.Logger
	waf     coraza.WAF
	poolKey string
}

// CaddyModule returns the Caddy module information.
func (corazaModule) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.waf",
		New: func() caddy.Module { return new(corazaModule) },
	}
}

// Provision implements caddy.Provisioner.
func (m *corazaModule) Provision(ctx caddy.Context) error {
	m.logger = zap.NewNop()
	if m.Deployment != "" {
		if err := sdkclient.ValidateShieldDeployment(m.Deployment); err != nil {
			return err
		}
		if m.ShieldMode != "audit" && m.ShieldMode != "enforce" {
			return fmt.Errorf("invalid Shield audit mode")
		}
		shield.RegisterWAFAggregates(ctx)
	}
	m.poolKey = m.computePoolKey()

	val, loaded, err := wafPool.LoadOrNew(m.poolKey, func() (caddy.Destructor, error) {
		waf, err := m.buildWAF()
		if err != nil {
			return nil, err
		}
		return &pooledWAF{waf: waf}, nil
	})
	if err != nil {
		return err
	}

	m.waf = val.(*pooledWAF).waf
	if loaded {
		m.logger.Info("reusing existing WAF instance from pool")
	}
	return nil
}

// buildWAF creates a new coraza.WAF from the module's configuration.
func (m *corazaModule) buildWAF() (coraza.WAF, error) {
	config := coraza.NewWAFConfig().
		WithErrorCallback(newErrorCb(m.logger)).
		WithDebugLogger(privacyLogger{})

	if m.LoadOWASPCRS {
		config = config.WithRootFS(mergefs.Merge(coreruleset.FS, mergefsio.OSFS))
	}

	if m.Directives != "" {
		config = config.WithDirectives(m.Directives)
	}

	if len(m.Include) > 0 {
		m.logger.Warn("'include' field is deprecated, please use the Include directive inside 'directives' field instead")
		for _, file := range m.Include {
			if strings.Contains(file, "*") {
				m.logger.Debug("Preparing to expand glob", zap.String("pattern", file))
				// we get files as expandables globs (with wildcard patterns)
				fs, err := filepath.Glob(file)
				if err != nil {
					return nil, err
				}
				m.logger.Debug("Glob expanded", zap.String("pattern", file), zap.Strings("files", fs))
				for _, f := range fs {
					config = config.WithDirectivesFromFile(f)
				}
			} else {
				m.logger.Debug("File was not a pattern, compiling it", zap.String("file", file))
				config = config.WithDirectivesFromFile(file)
			}
		}
	}

	return coraza.NewWAF(config)
}

// computePoolKey returns a deterministic key derived from the configuration
// fields that affect WAF construction. Two modules with identical configs
// will produce the same key, enabling WAF reuse across reloads.
func (m *corazaModule) computePoolKey() string {
	h := sha256.New()
	h.Write([]byte(m.Directives))
	h.Write([]byte{0}) // separator

	sorted := make([]string, len(m.Include))
	copy(sorted, m.Include)
	sort.Strings(sorted)
	for _, inc := range sorted {
		h.Write([]byte(inc))
		h.Write([]byte{0})
	}

	if m.LoadOWASPCRS {
		h.Write([]byte("crs"))
	}
	return fmt.Sprintf("coraza-waf-%x", h.Sum(nil))
}

// Validate implements caddy.Validator.
func (m *corazaModule) Validate() error {
	return nil
}

// Cleanup implements caddy.CleanerUpper.
func (m *corazaModule) Cleanup() error {
	_, err := wafPool.Delete(m.poolKey)
	return err
}

// isValidTxID reports whether s is acceptable as a transaction ID supplied by
// an upstream proxy. Coraza's concurrent audit log writer interpolates the
// transaction ID into a file path, so anything outside a conservative
// alphanumeric set is rejected to keep a spoofed header from escaping the
// audit log directory or forging log lines.
func isValidTxID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m corazaModule) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	id := randomString(16)
	if header := strings.TrimSpace(m.TxIDReqHeader); header != "" {
		if candidate := strings.TrimSpace(r.Header.Get(header)); isValidTxID(candidate) {
			id = candidate
		}
	}

	tx := m.waf.NewTransactionWithID(id)
	defer func() {
		tx.ProcessLogging()
		m.recordAggregates(tx)
		if err := tx.Close(); err != nil {
			m.logger.Warn("Failed to close the transaction", zap.String("tx_id", tx.ID()), zap.Error(err))
		}
	}()

	// Early return, Coraza is not going to process any rule
	if tx.IsRuleEngineOff() {
		// response writer is not going to be wrapped, but used as-is
		// to generate the response
		return next.ServeHTTP(w, r)
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	repl.Set("http.transaction_id", id)

	server := r.Context().Value(caddyhttp.ServerCtxKey).(*caddyhttp.Server)
	caddyhttp.PrepareRequest(r, repl, w, server)

	// ProcessRequest is just a wrapper around ProcessConnection, ProcessURI,
	// ProcessRequestHeaders and ProcessRequestBody.
	// It fails if any of these functions returns an error and it stops on interruption.
	if it, err := processRequest(tx, r); err != nil {
		// Inspection errors are not passed to Caddy's request logger either.
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	} else if it != nil {

		// A Caddy HandlerError logs the full request at DEBUG, including visitor
		// IP/URI/headers. A WAF decision completes the response without that path.
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(obtainStatusCodeFromInterruptionOrDefault(it, http.StatusOK))
		return nil
	}

	ww, processResponse := wrap(w, r, tx)

	// We continue with the other middlewares by catching the response
	if err := next.ServeHTTP(ww, r); err != nil {
		return err
	}

	// The interceptor has already set the failure status. Do not hand its
	// internal errors to the request logger, which includes visitor metadata.
	_ = processResponse(tx, r)
	return nil
}

// Unmarshal Caddyfile implements caddyfile.Unmarshaler.
func (m *corazaModule) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() {
		return d.Err("expected token following filter")
	}
	m.Include = []string{}
	for d.NextBlock(0) {
		key := d.Val()
		switch key {
		case "deployment":
			if !d.AllArgs(&m.Deployment) {
				return d.ArgErr()
			}
		case "shield_mode":
			if !d.AllArgs(&m.ShieldMode) {
				return d.ArgErr()
			}
		case "load_owasp_crs":
			if d.NextArg() {
				return d.ArgErr()
			}
			m.LoadOWASPCRS = true
		case "directives", "include":
			var value string
			if !d.Args(&value) {
				// not enough args
				return d.ArgErr()
			}

			if d.NextArg() {
				// too many args
				return d.ArgErr()
			}

			switch key {
			case "include":
				m.Include = append(m.Include, value)
			case "directives":
				m.Directives = value
			}
		case "tx_id_req_header":
			var value string
			if !d.Args(&value) {
				// not enough args
				return d.ArgErr()
			}

			if d.NextArg() {
				// too many args
				return d.ArgErr()
			}

			m.TxIDReqHeader = value
		default:
			return d.Errf("invalid key %q", key)
		}
	}

	return nil
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m corazaModule
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// No formatting of native match logs: they contain visitor metadata.
func newErrorCb(_ *zap.Logger) func(types.MatchedRule) { return func(types.MatchedRule) {} }

// The aggregate boundary accepts static IDs only. Nothing from a matched
// variable, expanded message or request is copied out of the transaction.
func (m corazaModule) recordAggregates(tx types.Transaction) {
	if m.Deployment == "" {
		return
	}
	ids := make([]int, 0, 16)
	wouldBlock := false
	for _, match := range tx.MatchedRules() {
		id := match.Rule().ID()
		meta, ok := sdkclient.ShieldRule(id)
		if !ok {
			continue
		}
		wouldBlock = wouldBlock || meta.Blocking
		if meta.Detection {
			ids = append(ids, id)
		}
	}
	outcome := "matched"
	if tx.IsInterrupted() {
		outcome = "blocked"
	} else if m.ShieldMode == "audit" && wouldBlock {
		outcome = "would_block"
	}
	shield.RecordWAFAggregates(m.Deployment, ids, outcome)
}

// Interface guards
var (
	_ caddy.Provisioner           = (*corazaModule)(nil)
	_ caddy.Validator             = (*corazaModule)(nil)
	_ caddy.CleanerUpper          = (*corazaModule)(nil)
	_ caddyhttp.MiddlewareHandler = (*corazaModule)(nil)
	_ caddyfile.Unmarshaler       = (*corazaModule)(nil)
)

// Modified by Impreza Host from the upstream connector; see UPSTREAM.md.
