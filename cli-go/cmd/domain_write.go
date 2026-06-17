package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// Phase 7.5.1 — 9 advanced domain write verbs that close the parity
// gap with the Python CLI:
//
//	register / transfer / set-nameservers
//	lock / unlock
//	id-protection
//	raa-verify / gdpr-auth / transfer-approval
//
// All mutating verbs gate on confirm (--yes / -y skips). Cost-charging
// verbs do a best-effort catalog price lookup so the confirmation
// prompt shows the impact upfront. InsufficientCredit (402) gets a
// tailored hint pointing at `impreza account topup` — same pattern
// as the Python CLI.

// ── domain register ──────────────────────────────────────────────

var (
	domainRegisterYears       int
	domainRegisterNameservers []string
)

var domainRegisterCmd = &cobra.Command{
	Use:   "register <domain>",
	Short: "Register a new domain. Charges from account balance.",
	Long: `Register a new domain through Impreza Host.

The cost is charged from your account balance. Top up first if needed:
  impreza account topup --amount 50 --method xmr

Nameservers default to Impreza's nameservers; pass --ns repeatedly
to set custom ones at registration time:
  impreza domain register example.com --years 2 --ns ns1.foo --ns ns2.foo`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		domain := args[0]
		if domainRegisterYears < 1 || domainRegisterYears > 10 {
			return errors.New("--years must be between 1 and 10")
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}

		// Best-effort price + balance lookup so the prompt shows the cost.
		prompt := fmt.Sprintf("Register %q for %d year(s). Cost is charged from your account balance.",
			domain, domainRegisterYears)
		if amount, currency, ok := tryLookupRegisterPrice(cmd, c, domain, domainRegisterYears); ok {
			prompt = fmt.Sprintf("Register %q for %d year(s) — %.2f %s from your balance%s.",
				domain, domainRegisterYears, amount, currency, accountBalanceSuffix(cmd, c))
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt, autoYes); err != nil {
			return err
		}

		req := client.DomainRegisterRequest{
			Domain:      domain,
			Years:       domainRegisterYears,
			Nameservers: domainRegisterNameservers,
		}
		res, err := c.DomainRegister(cmd.Context(), req)
		if err != nil {
			return hintInsufficientCredit(err)
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}
		output.Success("Registered %q — order #%d, invoice #%d, charged %.2f %s.",
			res.Domain, res.OrderID, res.InvoiceID, res.Amount, res.Currency)
		if res.Status != "" {
			output.Info("Status: %s.", res.Status)
		}
		return nil
	},
}

// ── domain transfer ──────────────────────────────────────────────

var (
	domainTransferEpp   string
	domainTransferYears int
)

var domainTransferCmd = &cobra.Command{
	Use:   "transfer <domain>",
	Short: "Transfer a domain in from another registrar. Charges from balance.",
	Long: `Transfer a domain in. Requires the EPP / authorisation code from
the losing registrar (run ` + "`impreza domain unlock`" + ` there first, or
its equivalent). Most TLDs have a 5-7 day transfer window during
which the gaining registrar (Impreza) contacts the losing one — see
` + "`impreza domain show <d>`" + ` for progress after initiation.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		domain := args[0]
		if domainTransferEpp == "" {
			return errors.New("--epp is required (authorisation code from the current registrar)")
		}
		if domainTransferYears < 1 || domainTransferYears > 10 {
			return errors.New("--years must be between 1 and 10")
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		prompt := fmt.Sprintf("Transfer %q in (renewal: %d year). Cost is charged from your balance.",
			domain, domainTransferYears)
		if amount, currency, ok := tryLookupRegisterPrice(cmd, c, domain, domainTransferYears); ok {
			prompt = fmt.Sprintf("Transfer %q in (renewal: %d year) — ~%.2f %s from your balance%s.",
				domain, domainTransferYears, amount, currency, accountBalanceSuffix(cmd, c))
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt, autoYes); err != nil {
			return err
		}

		req := client.DomainTransferRequest{
			Domain:  domain,
			EppCode: domainTransferEpp,
			Years:   domainTransferYears,
		}
		res, err := c.DomainTransfer(cmd.Context(), req)
		if err != nil {
			return hintInsufficientCredit(err)
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}
		output.Success("Transfer initiated for %q — order #%d, invoice #%d, charged %.2f %s.",
			res.Domain, res.OrderID, res.InvoiceID, res.Amount, res.Currency)
		output.Info("Track progress with: impreza domain show %s", res.Domain)
		return nil
	},
}

// ── domain set-nameservers ───────────────────────────────────────

var domainSetNameserversCmd = &cobra.Command{
	Use:   "set-nameservers <domain> <ns1> <ns2> [<ns3>...]",
	Short: "Replace the domain's nameservers (minimum 2).",
	Long: `Replace the domain's nameservers at the registry. Propagation across
the DNS hierarchy can take up to 48h after this returns; the registry
update itself is immediate.`,
	Args: cobra.MinimumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		domain := args[0]
		nameservers := args[1:]
		if len(nameservers) < 2 {
			return errors.New("at least 2 nameservers are required")
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainSetNameservers(cmd.Context(), domain, nameservers); err != nil {
			return err
		}
		output.Success("Nameservers for %q set to: %s.", domain, strings.Join(nameservers, ", "))
		return nil
	},
}

// ── domain lock / unlock ─────────────────────────────────────────

var domainLockCmd = &cobra.Command{
	Use:   "lock <domain>",
	Short: "Enable transfer lock (prevents the domain from being moved away).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainLock(cmd.Context(), args[0]); err != nil {
			return err
		}
		output.Success("Transfer lock enabled on %q.", args[0])
		return nil
	},
}

var domainUnlockCmd = &cobra.Command{
	Use:   "unlock <domain>",
	Short: "Disable transfer lock and print the EPP / authorisation code.",
	Long: `Unlock the domain at the registry and print the EPP / authorisation
code that anyone needs to transfer the domain to another registrar.

This is a sensitive action — once printed, the EPP code authorises
transfers away from Impreza for anyone who holds it. Re-lock with
` + "`impreza domain lock`" + ` after you're done.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		domain := args[0]
		prompt := fmt.Sprintf(
			"Unlocking %q returns the EPP code, which authorises transfers "+
				"away from Impreza. Anyone with the code can move the domain.",
			domain)
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt, autoYes); err != nil {
			return err
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		epp, err := c.DomainUnlock(cmd.Context(), domain)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), map[string]any{"epp_code": epp}, f)
		}
		output.Success("Transfer lock disabled on %q.", domain)
		output.Info("  EPP / auth code: %s", epp)
		return nil
	},
}

// ── domain id-protection ─────────────────────────────────────────

var domainIDProtectionCmd = &cobra.Command{
	Use:   "id-protection <domain>",
	Short: "Purchase WHOIS Privacy / ID protection. Charges from balance.",
	Long: `Purchase WHOIS Privacy to hide the registrant contact details from
public WHOIS lookups. Some TLDs (e.g. .us, certain ccTLDs) don't
support privacy at the registry level — the API returns 400 with a
descriptive message in that case.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		domain := args[0]
		prompt := fmt.Sprintf("Purchase ID protection for %q. Cost is charged from your account balance.", domain)
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt, autoYes); err != nil {
			return err
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		res, err := c.DomainPurchaseIDProtection(cmd.Context(), domain)
		if err != nil {
			return hintInsufficientCredit(err)
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}
		if len(res) == 0 {
			output.Success("ID protection purchased for %q.", domain)
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		for k, v := range res {
			t.AppendRow(table.Row{k, fmt.Sprintf("%v", v)})
		}
		t.Render()
		return nil
	},
}

// ── domain raa-verify / gdpr-auth / transfer-approval ────────────

var domainRAAVerifyCmd = &cobra.Command{
	Use:   "raa-verify <domain>",
	Short: "Resend the ICANN RAA email-verification message.",
	Long: `Resend the ICANN RAA verification email to the registrant address.
Required after registration to confirm the email is valid; without
verification ICANN suspends the domain after 15 days.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainResendRAAVerification(cmd.Context(), args[0]); err != nil {
			return err
		}
		output.Success("RAA verification email resent for %q.", args[0])
		return nil
	},
}

var domainGDPRAuthCmd = &cobra.Command{
	Use:   "gdpr-auth <domain>",
	Short: "Resend the GDPR data-processing authorisation email.",
	Long:  `Resend the GDPR data-processing authorisation email. Required for EU-resident registrants on certain TLDs.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainResendGDPRAuth(cmd.Context(), args[0]); err != nil {
			return err
		}
		output.Success("GDPR authorisation email resent for %q.", args[0])
		return nil
	},
}

var domainTransferApprovalCmd = &cobra.Command{
	Use:   "transfer-approval <domain>",
	Short: "Resend the inbound-transfer approval email.",
	Long: `Resend the inbound-transfer approval email. Sent to the registrant's
WHOIS email by the gaining registrar; users sometimes miss it, this
command resends.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainResendTransferApproval(cmd.Context(), args[0]); err != nil {
			return err
		}
		output.Success("Transfer-approval email resent for %q.", args[0])
		return nil
	},
}

// ── helpers ──────────────────────────────────────────────────────

// tryLookupRegisterPrice is a best-effort price lookup so the
// register / transfer confirmation prompts can show the cost
// upfront. Returns ok=false on any error — the verb still runs,
// users just see the generic "cost will be charged" message.
//
// Matches the Python CLI's `_try_lookup_register_price` semantics:
// extracts the TLD, queries the catalog for that single TLD, picks
// the per-year price for the requested term, multiplies if only
// year-1 is priced. Naive multi-year — surface the caveat in the
// prompt with the "~" prefix.
func tryLookupRegisterPrice(cmd *cobra.Command, c *client.Client, domain string, years int) (float64, string, bool) {
	idx := strings.Index(domain, ".")
	if idx < 0 || idx == len(domain)-1 {
		return 0, "", false
	}
	tld := domain[idx+1:]

	row, err := c.DomainPricing(cmd.Context(), tld)
	if err != nil {
		return 0, "", false
	}
	if row == nil {
		return 0, "", false
	}
	// Map keys are stringified year ints in the catalog response.
	key := fmt.Sprintf("%d", years)
	if v, ok := row.Register[key]; ok && v > 0 {
		return v, row.Currency, true
	}
	if v, ok := row.Register["1"]; ok && v > 0 {
		return v * float64(years), row.Currency, true
	}
	return 0, "", false
}

// accountBalanceSuffix returns " (balance: X.XX CCY)" or "" on
// lookup failure. Used to add balance context to confirmation prompts
// for cost-charging verbs without making the verb depend on a
// successful lookup.
func accountBalanceSuffix(cmd *cobra.Command, c *client.Client) string {
	info, err := c.AccountInfo(cmd.Context())
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (balance: %.2f %s)", info.Balance, info.Currency)
}

// hintInsufficientCredit checks whether err is a 402 InsufficientCredit
// API error and, if so, returns it wrapped with a remediation hint
// pointing at `impreza account topup`. Otherwise returns err unchanged.
// Mirrors the Python CLI's `_exit_on_insufficient_credit` helper.
func hintInsufficientCredit(err error) error {
	if err == nil {
		return nil
	}
	var ic *client.InsufficientCredit
	if errors.As(err, &ic) {
		return fmt.Errorf("%w\n  -> Top up your balance with: impreza account topup --amount <X> --method btc|xmr|trx|usdt", err)
	}
	return err
}

func init() {
	// Cost-charging verbs gate on confirm by default; --yes / -y skips.
	domainRegisterCmd.Flags().IntVar(&domainRegisterYears, "years", 1, "Registration period (1-10).")
	domainRegisterCmd.Flags().StringArrayVar(&domainRegisterNameservers, "ns", nil,
		"Nameserver hostname. Repeat to set multiple; defaults to Impreza NS when omitted.")
	domainRegisterCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	domainTransferCmd.Flags().StringVar(&domainTransferEpp, "epp", "",
		"Authorisation / EPP code from the current registrar (required).")
	domainTransferCmd.Flags().IntVar(&domainTransferYears, "years", 1, "Renewal period to add (1-10).")
	domainTransferCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	domainUnlockCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the EPP-warning prompt.")
	domainIDProtectionCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	domainCmd.AddCommand(
		domainRegisterCmd,
		domainTransferCmd,
		domainSetNameserversCmd,
		domainLockCmd,
		domainUnlockCmd,
		domainIDProtectionCmd,
		domainRAAVerifyCmd,
		domainGDPRAuthCmd,
		domainTransferApprovalCmd,
	)
}
