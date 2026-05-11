package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var catalogCmd = &cobra.Command{
	Use:   "catalog",
	Short: "Browse the product catalog (pre-purchase discovery).",
	Long: `Catalog endpoints read the static product catalogue staff curate
in Impreza Account. Use these before placing an order to find the
right product / group / TLD pricing.`,
}

// ── catalog products ─────────────────────────────────────────────

var catalogProductsGroup string

var catalogProductsCmd = &cobra.Command{
	Use:   "products",
	Short: "List all available products.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		ps, err := c.CatalogProducts(cmd.Context(), catalogProductsGroup)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), ps, f)
		}

		if len(ps) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No products in this catalog filter.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "name", "group", "type", "monthly", "annually"})
		for _, p := range ps {
			monthly := ""
			annually := ""
			if mp, ok := p.Pricing["monthly"]; ok && mp.Price > 0 {
				monthly = fmt.Sprintf("%.2f %s", mp.Price, p.Currency)
			}
			if ap, ok := p.Pricing["annually"]; ok && ap.Price > 0 {
				annually = fmt.Sprintf("%.2f %s", ap.Price, p.Currency)
			}
			t.AppendRow(table.Row{p.ID, p.Name, p.Group, p.Type, monthly, annually})
		}
		t.Render()
		return nil
	},
}

// ── catalog product <id> ─────────────────────────────────────────

var catalogProductCmd = &cobra.Command{
	Use:   "product <id>",
	Short: "Show one product with its config options + custom fields.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("product id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		p, err := c.CatalogProduct(cmd.Context(), id)
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

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", p.ID})
		t.AppendRow(table.Row{"name", p.Name})
		if p.Group != "" {
			t.AppendRow(table.Row{"group", p.Group})
		}
		if p.Type != "" {
			t.AppendRow(table.Row{"type", p.Type})
		}
		if p.Currency != "" {
			t.AppendRow(table.Row{"currency", p.Currency})
		}
		if p.Description != "" {
			t.AppendRow(table.Row{"description", p.Description})
		}
		for cycle, pp := range p.Pricing {
			if pp.Price > 0 {
				t.AppendRow(table.Row{"price." + cycle, fmt.Sprintf("%.2f %s", pp.Price, p.Currency)})
			}
		}
		t.AppendRow(table.Row{"config_options", len(p.ConfigOptions)})
		t.AppendRow(table.Row{"custom_fields", len(p.CustomFields)})
		t.Render()
		return nil
	},
}

// ── catalog product-groups ───────────────────────────────────────

var catalogProductGroupsCmd = &cobra.Command{
	Use:   "product-groups",
	Short: "List product group names + slugs.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		gs, err := c.CatalogProductGroups(cmd.Context())
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), gs, f)
		}

		if len(gs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No product groups.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "name", "slug"})
		for _, g := range gs {
			t.AppendRow(table.Row{g.ID, g.Name, g.Slug})
		}
		t.Render()
		return nil
	},
}

// ── catalog tlds ─────────────────────────────────────────────────

var catalogTldsFilter string

var catalogTldsCmd = &cobra.Command{
	Use:   "tlds",
	Short: "List supported TLDs with register/transfer/renew pricing.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		var filter []string
		if catalogTldsFilter != "" {
			for _, t := range strings.Split(catalogTldsFilter, ",") {
				if v := strings.TrimSpace(strings.TrimPrefix(t, ".")); v != "" {
					filter = append(filter, v)
				}
			}
		}
		ts, err := c.CatalogTlds(cmd.Context(), filter)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), ts, f)
		}

		if len(ts) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No matching TLDs.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"tld", "register (1y)", "renew (1y)", "min_years", "currency"})
		for _, row := range ts {
			tld := row.TLD
			if !strings.HasPrefix(tld, ".") {
				tld = "." + tld
			}
			reg := ""
			if v, ok := row.Register["1"]; ok {
				reg = fmt.Sprintf("%.2f", v)
			}
			ren := ""
			if v, ok := row.Renew["1"]; ok {
				ren = fmt.Sprintf("%.2f", v)
			}
			my := ""
			if row.MinYears > 0 {
				my = strconv.Itoa(row.MinYears)
			}
			t.AppendRow(table.Row{tld, reg, ren, my, row.Currency})
		}
		t.Render()
		return nil
	},
}

func init() {
	catalogProductsCmd.Flags().StringVar(&catalogProductsGroup, "group", "",
		"Filter by group name (e.g. 'VPS', 'Hosting').")
	catalogTldsCmd.Flags().StringVar(&catalogTldsFilter, "filter", "",
		"Comma-separated TLD filter (e.g. 'com,net,org' — leading dots optional).")

	catalogCmd.AddCommand(catalogProductsCmd, catalogProductCmd, catalogProductGroupsCmd, catalogTldsCmd)
	rootCmd.AddCommand(catalogCmd)
}
