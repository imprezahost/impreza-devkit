package cmd

import (
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var domainCmd = &cobra.Command{
	Use:   "domain",
	Short: "Read domain registrations, availability, pricing, and DNS records.",
}

// ── domain show <name> ───────────────────────────────────────────

var domainShowCmd = &cobra.Command{
	Use:   "show <domain>",
	Short: "Show the registration record for a domain you own.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		d, err := c.DomainShow(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), d, f)
		}

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"domain", d.Domain})
		t.AppendRow(table.Row{"status", d.Status})
		t.AppendRow(table.Row{"registrar", d.Registrar})
		t.AppendRow(table.Row{"registered_at", d.RegisteredAt})
		t.AppendRow(table.Row{"expires_at", d.ExpiresAt})
		t.AppendRow(table.Row{"auto_renew", d.AutoRenew})
		t.AppendRow(table.Row{"lock", d.Lock})
		t.AppendRow(table.Row{"id_protection", d.IDProtection})
		if len(d.Nameservers) > 0 {
			t.AppendRow(table.Row{"nameservers", strings.Join(d.Nameservers, ", ")})
		}
		t.Render()
		return nil
	},
}

// ── domain check <name>... ───────────────────────────────────────

var domainCheckCmd = &cobra.Command{
	Use:   "check <domain> [<domain>...]",
	Short: "Check availability + price for one or more domain names.",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		res, err := c.DomainCheck(cmd.Context(), args)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"domain", "available", "premium", "price"})
		for _, r := range res {
			priceCell := ""
			if r.Price > 0 {
				priceCell = fmt.Sprintf("%.2f %s", r.Price, r.Currency)
			}
			t.AppendRow(table.Row{r.Domain, r.Available, r.Premium, priceCell})
		}
		t.Render()
		return nil
	},
}

// ── domain pricing <tld> ──────────────────────────────────────────

var domainPricingCmd = &cobra.Command{
	Use:   "pricing <tld>",
	Short: "Show register / transfer / renewal pricing for one TLD.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		p, err := c.DomainPricing(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), p, f)
		}

		tld := p.TLD
		if len(tld) > 0 && tld[0] != '.' {
			tld = "." + tld
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"tld", tld})
		t.AppendRow(table.Row{"currency", p.Currency})
		if p.MinYears > 0 {
			t.AppendRow(table.Row{"min_years", p.MinYears})
		}
		if p.Cheapest > 0 {
			t.AppendRow(table.Row{"cheapest", fmt.Sprintf("%.2f", p.Cheapest)})
		}
		// Multi-year register prices, sorted by year.
		for y := 1; y <= 10; y++ {
			if v, ok := p.Register[fmt.Sprintf("%d", y)]; ok {
				t.AppendRow(table.Row{fmt.Sprintf("register.%dy", y), fmt.Sprintf("%.2f", v)})
			}
		}
		for y := 1; y <= 10; y++ {
			if v, ok := p.Renew[fmt.Sprintf("%d", y)]; ok {
				t.AppendRow(table.Row{fmt.Sprintf("renew.%dy", y), fmt.Sprintf("%.2f", v)})
			}
		}
		t.Render()
		return nil
	},
}

// ── domain dns list <name> ────────────────────────────────────────

var domainDnsCmd = &cobra.Command{
	Use:   "dns",
	Short: "DNS records for a domain (list / add / update / delete in 7.3).",
}

var domainDnsListCmd = &cobra.Command{
	Use:   "list <domain>",
	Short: "List every DNS record for a domain.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		recs, err := c.DomainDnsList(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), recs, f)
		}

		if len(recs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No DNS records.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"type", "host", "value", "ttl", "priority"})
		for _, r := range recs {
			val := r.Value
			if len(val) > 60 {
				val = val[:57] + "..."
			}
			t.AppendRow(table.Row{r.Type, r.DisplayName(), val, r.TTL, r.Priority})
		}
		t.Render()
		return nil
	},
}

func init() {
	domainDnsCmd.AddCommand(domainDnsListCmd)
	domainCmd.AddCommand(domainShowCmd, domainCheckCmd, domainPricingCmd, domainDnsCmd)
	rootCmd.AddCommand(domainCmd)
}
