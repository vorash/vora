package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vorash/vora/internal/client"
)

// secretMaxValue matches the control plane's accepted credential size.
const secretMaxValue = 16 << 10

// apiClient returns a remote client, or an error if --api is not set.
func (a *app) apiClient() (*client.HTTPClient, error) {
	if a.api == "" {
		return nil, fmt.Errorf("this command requires --api (the vora-api base URL)")
	}
	return client.DialAPI(a.api, a.apiKey)
}

func (a *app) runnersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runners",
		Short: "List runners registered with vora-api (requires --api)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			runners, err := c.ListRunners(ctx)
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOutAny(map[string]any{"runners": runners})
			}
			if len(runners) == 0 {
				fmt.Println("no runners registered")
				return nil
			}
			fmt.Printf("%-26s %-22s %-8s %-8s %s\n", "ID", "ADDRESS", "HEALTHY", "RUNNING", "BACKENDS")
			for _, r := range runners {
				fmt.Printf("%-26s %-22s %-8v %-8d %s\n", r.ID, r.Address, r.Healthy, r.Running, strings.Join(r.Backends, ","))
			}
			return nil
		},
	}
}

func (a *app) keysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage vora-api keys (requires --api)",
	}
	cmd.AddCommand(a.keysCreateCmd(), a.keysListCmd(), a.keysRevokeCmd())
	return cmd
}

func (a *app) secretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage brokered credentials for restricted sandboxes (requires --api)",
	}
	cmd.AddCommand(a.secretsSetCmd(), a.secretsListCmd(), a.secretsRmCmd())
	return cmd
}

func (a *app) secretsSetCmd() *cobra.Command {
	var (
		alias, host, scheme, header, value, tenant string
		stdin                                      bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Store a credential under an alias (the value is never shown again)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if stdin {
				if value != "" {
					return fmt.Errorf("--value and --value-stdin are mutually exclusive")
				}
				data, err := io.ReadAll(io.LimitReader(os.Stdin, secretMaxValue+1))
				if err != nil {
					return err
				}
				if len(data) > secretMaxValue {
					return fmt.Errorf("stdin value exceeds %d bytes", secretMaxValue)
				}
				value = strings.TrimRight(string(data), "\r\n")
			}
			if value == "" {
				return fmt.Errorf("provide --value or --value-stdin")
			}
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stored, err := c.CreateSecret(ctx, alias, host, scheme, header, value, tenant)
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOutAny(map[string]any{"secret": stored})
			}
			fmt.Printf("secret %s bound to %s (%s)\n", stored.Alias, stored.Host, stored.Scheme)
			return nil
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "alias the sandbox uses in the broker route (required)")
	cmd.Flags().StringVar(&host, "host", "", "upstream host the credential is bound to (required)")
	cmd.Flags().StringVar(&scheme, "scheme", "bearer", "attachment scheme: bearer or header")
	cmd.Flags().StringVar(&header, "header", "", "header name for --scheme header")
	cmd.Flags().StringVar(&value, "value", "", "credential value (visible in shell history; prefer --value-stdin)")
	cmd.Flags().BoolVar(&stdin, "value-stdin", false, "read the credential value from stdin")
	cmd.Flags().StringVar(&tenant, "tenant", "", "tenant id (global administrators only)")
	_ = cmd.MarkFlagRequired("alias")
	_ = cmd.MarkFlagRequired("host")
	return cmd
}

func (a *app) secretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List credential metadata (values are never returned)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			secrets, err := c.ListSecrets(ctx)
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOutAny(map[string]any{"secrets": secrets})
			}
			if len(secrets) == 0 {
				fmt.Println("no secrets")
				return nil
			}
			fmt.Printf("%-16s %-28s %-8s %s\n", "ALIAS", "HOST", "SCHEME", "UPDATED")
			for _, s := range secrets {
				fmt.Printf("%-16s %-28s %-8s %s\n", s.Alias, s.Host, s.Scheme, s.UpdatedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func (a *app) secretsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <alias>",
		Short: "Delete a stored credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.DeleteSecret(ctx, args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed %s\n", args[0])
			return nil
		},
	}
}

func (a *app) keysCreateCmd() *cobra.Command {
	var (
		name   string
		tenant string
		scopes []string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an API key (prints the key once)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			plaintext, info, err := c.CreateKey(ctx, name, tenant, scopes)
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOutAny(map[string]any{"key": plaintext, "api_key": info})
			}
			fmt.Printf("key:    %s\n", plaintext)
			fmt.Printf("id:     %s\n", info.ID)
			fmt.Printf("scopes: %s\n", strings.Join(info.Scopes, ","))
			fmt.Println("store this key now; it is not shown again")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human-readable key name")
	cmd.Flags().StringVar(&tenant, "tenant", "", "tenant id (defaults to the caller's)")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "scope: admin or sandbox (repeatable; default sandbox)")
	return cmd
}

func (a *app) keysListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API keys",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			keys, err := c.ListKeys(ctx)
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOutAny(map[string]any{"keys": keys})
			}
			if len(keys) == 0 {
				fmt.Println("no keys")
				return nil
			}
			fmt.Printf("%-26s %-16s %-10s %-8s %s\n", "ID", "NAME", "REVOKED", "TENANT", "SCOPES")
			for _, k := range keys {
				fmt.Printf("%-26s %-16s %-10v %-8s %s\n", k.ID, k.Name, k.Revoked, k.TenantID, strings.Join(k.Scopes, ","))
			}
			return nil
		},
	}
}

func (a *app) keysRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := a.apiClient()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.RevokeKey(ctx, args[0]); err != nil {
				return err
			}
			fmt.Printf("Revoked %s\n", args[0])
			return nil
		},
	}
}

// jsonOutAny prints any value as indented JSON.
func (a *app) jsonOutAny(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
