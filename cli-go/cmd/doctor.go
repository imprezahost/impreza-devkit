package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/config"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// `impreza doctor` runs five sequenced health checks. Exit 0 only if
// all pass. Mirrors the Python CLI's doctor — same check names, same
// labels, same exit semantics.

const (
	doctorOK   = "[OK]"
	doctorFail = "[FAIL]"
	doctorWarn = "[WARN]"
	doctorSkip = "[SKIP]"
)

type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // OK | FAIL | WARN | SKIP (no brackets)
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	OK      bool   `json:"ok"`
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Five-check health verification. Exits 0 only if all checks pass.",
	Long: `Sequenced health check:

  1. active-context: a context is configured + selected (no network).
  2. api-reachable:  GET /account/api-keys/self returns 200 OK.
  3. key-status:     the API key is active (not paused / revoked).
  4. ip-whitelist:   the request_ip the server saw is on the key whitelist.
  5. account-profile: GET /account returns the profile + balance.

Each check renders as [OK] / [FAIL] / [WARN] / [SKIP]. Exit code:

  0 — every check passed
  1 — any check failed

Use this as a smoke test after install ("does my context work?"),
in CI ("are credentials still valid?"), or as a first-line support
artefact ("paste the impreza doctor output").`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		checks := runDoctorChecks(cmd)

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(out, checks, f)
		}

		writeDoctorReport(out, checks)

		// Exit non-zero if any check failed.
		failed := 0
		for _, c := range checks {
			if !c.OK {
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("doctor: %d of %d checks failed", failed, len(checks))
		}
		return nil
	},
}

func runDoctorChecks(cmd *cobra.Command) []doctorCheck {
	out := []doctorCheck{}

	// ── 1. active-context (no network) ────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		out = append(out, doctorCheck{
			Name:    "active-context",
			Status:  "FAIL",
			Summary: "No config file found",
			Detail:  fmt.Sprintf("%v\n\nRun `impreza context create <name> --key imp_... --secret ...` to set up your first context.", err),
			OK:      false,
		})
		// Subsequent checks need a context; mark them SKIP.
		for _, n := range []string{"api-reachable", "key-status", "ip-whitelist", "account-profile"} {
			out = append(out, doctorCheck{Name: n, Status: "SKIP", Summary: "no active context"})
		}
		return out
	}
	name, ctx, err := cfg.Active(globalContext)
	if err != nil {
		out = append(out, doctorCheck{
			Name:    "active-context",
			Status:  "FAIL",
			Summary: "No default context set and no --context override",
			Detail:  fmt.Sprintf("%v\n\nRun `impreza context use <name>` or pass --context <name>.", err),
			OK:      false,
		})
		for _, n := range []string{"api-reachable", "key-status", "ip-whitelist", "account-profile"} {
			out = append(out, doctorCheck{Name: n, Status: "SKIP", Summary: "no active context"})
		}
		return out
	}
	out = append(out, doctorCheck{
		Name:    "active-context",
		Status:  "OK",
		Summary: fmt.Sprintf("Active context: %s", name),
		OK:      true,
	})

	// ── 2. api-reachable + 3. key-status + 4. ip-whitelist
	//      (all three are derived from the same GET .../api-keys/self call).
	c, err := client.New(ctx)
	if err != nil {
		out = append(out, doctorCheck{
			Name: "api-reachable", Status: "FAIL",
			Summary: "Couldn't build HTTP client", Detail: err.Error(), OK: false,
		})
		return appendSkipChecks(out, "key-status", "ip-whitelist", "account-profile")
	}

	start := time.Now()
	id, err := c.ApiKeySelf(cmd.Context())
	elapsed := time.Since(start).Round(time.Millisecond)

	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) {
			switch ae.Status {
			case 401:
				out = append(out, doctorCheck{
					Name: "api-reachable", Status: "FAIL",
					Summary: "Authentication failed (HTTP 401)",
					Detail:  fmt.Sprintf("%s. The API key or secret is invalid. Rotate via your Impreza Account, update the local context with `impreza context create / use`.", ae.Message),
					OK:      false,
				})
			case 403:
				out = append(out, doctorCheck{
					Name: "api-reachable", Status: "FAIL",
					Summary: "Forbidden (HTTP 403)",
					Detail:  fmt.Sprintf("%s. Add the calling IP to the key's whitelist via your Impreza Account, or use a different key whose whitelist already covers this IP.", ae.Message),
					OK:      false,
				})
			default:
				out = append(out, doctorCheck{
					Name: "api-reachable", Status: "FAIL",
					Summary: fmt.Sprintf("HTTP %d", ae.Status),
					Detail:  ae.Error(),
					OK:      false,
				})
			}
		} else {
			out = append(out, doctorCheck{
				Name: "api-reachable", Status: "FAIL",
				Summary: "Network error", Detail: err.Error(), OK: false,
			})
		}
		return appendSkipChecks(out, "key-status", "ip-whitelist", "account-profile")
	}

	out = append(out, doctorCheck{
		Name:    "api-reachable",
		Status:  "OK",
		Summary: fmt.Sprintf("GET /account/api-keys/self OK (%s)", elapsed),
		Detail:  fmt.Sprintf("key prefix=%q, label=%q", id.Prefix, id.Label),
		OK:      true,
	})

	// ── 3. key-status
	keyOK := id.Status == "active"
	keyStatus := "OK"
	if !keyOK {
		keyStatus = "FAIL"
	}
	out = append(out, doctorCheck{
		Name:    "key-status",
		Status:  keyStatus,
		Summary: fmt.Sprintf("status=%q", id.Status),
		Detail: func() string {
			if keyOK {
				return ""
			}
			return "Pending, paused, or revoked keys will start returning 401 unpredictably. Rotate via your Impreza Account."
		}(),
		OK: keyOK,
	})

	// ── 4. ip-whitelist
	if id.RequestIP == "" {
		out = append(out, doctorCheck{
			Name:    "ip-whitelist",
			Status:  "WARN",
			Summary: "Server did not report request_ip",
			Detail:  "The /api-keys/self endpoint should echo the IP it saw the request from. Missing field is unusual but the request authenticated; inspect via your Impreza Account to be sure.",
			OK:      true, // warn but don't fail
		})
	} else {
		matched := false
		matchLabel := ""
		for _, e := range id.IPWhitelist {
			if e.IPAddress == id.RequestIP {
				matched = true
				matchLabel = e.Label
				if matchLabel == "" {
					matchLabel = "(no label)"
				}
				break
			}
		}
		if matched {
			out = append(out, doctorCheck{
				Name:    "ip-whitelist",
				Status:  "OK",
				Summary: fmt.Sprintf("request_ip %s matches entry (%s)", id.RequestIP, matchLabel),
				OK:      true,
			})
		} else {
			labels := []string{}
			for _, e := range id.IPWhitelist {
				labels = append(labels, e.IPAddress)
			}
			out = append(out, doctorCheck{
				Name:    "ip-whitelist",
				Status:  "FAIL",
				Summary: fmt.Sprintf("request_ip %s not in whitelist (%d entr%s)", id.RequestIP, len(id.IPWhitelist), pluralY(len(id.IPWhitelist))),
				Detail:  fmt.Sprintf("Whitelist: %v. Add the calling IP via your Impreza Account, or switch to a context whose key already allows this IP.", labels),
				OK:      false,
			})
		}
	}

	// ── 5. account-profile
	info, err := c.AccountInfo(cmd.Context())
	if err != nil {
		out = append(out, doctorCheck{
			Name: "account-profile", Status: "FAIL",
			Summary: "Couldn't fetch /account", Detail: err.Error(), OK: false,
		})
	} else {
		out = append(out, doctorCheck{
			Name:    "account-profile",
			Status:  "OK",
			Summary: fmt.Sprintf("%s %s <%s>, balance %.2f %s", info.FirstName, info.LastName, info.Email, info.Balance, info.Currency),
			Detail:  fmt.Sprintf("registered %s", info.RegisteredAt),
			OK:      true,
		})
	}

	return out
}

// pluralY returns "y" or "ies" for the singular/plural of "entry".
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// appendSkipChecks appends SKIP rows for the listed check names. Used
// when an earlier check fails and the rest can't usefully run.
func appendSkipChecks(checks []doctorCheck, names ...string) []doctorCheck {
	for _, n := range names {
		checks = append(checks, doctorCheck{
			Name: n, Status: "SKIP", Summary: "earlier check failed",
		})
	}
	return checks
}

// writeDoctorReport renders the table output with a top + bottom rule.
func writeDoctorReport(w io.Writer, checks []doctorCheck) {
	fmt.Fprintln(w, "impreza doctor")
	fmt.Fprintln(w, "----------------------------------------")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleDefault)
	style := t.Style()
	style.Options.DrawBorder = false
	style.Options.SeparateRows = false
	style.Options.SeparateColumns = false
	style.Options.SeparateHeader = false
	for _, c := range checks {
		label := doctorLabel(c.Status)
		row := []any{label, c.Name + ": " + c.Summary}
		t.AppendRow(row)
		if c.Detail != "" {
			t.AppendRow([]any{"", "        " + c.Detail})
		}
	}
	t.Render()
	fmt.Fprintln(w, "----------------------------------------")
	passed, failed := 0, 0
	for _, c := range checks {
		if c.Status == "SKIP" {
			continue
		}
		if c.OK {
			passed++
		} else {
			failed++
		}
	}
	if failed == 0 {
		fmt.Fprintf(w, "All checks passed. %d/%d.\n", passed, passed)
	} else {
		fmt.Fprintf(w, "FAILED: %d of %d checks failed.\n", failed, passed+failed)
	}
}

func doctorLabel(status string) string {
	switch status {
	case "OK":
		return doctorOK
	case "FAIL":
		return doctorFail
	case "WARN":
		return doctorWarn
	case "SKIP":
		return doctorSkip
	default:
		return "[" + status + "]"
	}
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
