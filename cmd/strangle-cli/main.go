// Command strangle-cli is the client for a strangle server. It queries the
// versioned HTTP API and provisions local accounts, API tokens and collection
// memberships in the server's database.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/utils/cfg"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "strangle-cli",
		Short: "Client commands for a strangle server.",
		// The error is printed by main; keep cobra from printing it too.
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: "strangle-cli is the client tool for strangle. The hosts, host " +
			"and hostkeys commands talk to a server through its versioned " +
			"HTTP API. The user, token and collection commands provision data " +
			"directly in the server's database; run them where the database is " +
			"reachable. To start a server, use strangle-server serve.",
	}

	root.AddCommand(userCmd())
	root.AddCommand(tokenCmd())
	root.AddCommand(collectionCmd())
	root.AddCommand(apiCommands()...)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// userCmd manages the local UI accounts stored in the database users table:
//
//	strangle-cli user add <name> --password <pw>
//	strangle-cli user add <name> --password-stdin
//	strangle-cli user list
//	strangle-cli user remove <name>
//
// Accounts are provisioned out of band, not through the API. Once the first
// account exists, the web UI requires a login.
func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage local UI accounts (stored in the database users table).",
	}
	// The accounts live in the configured database, so the subcommands need
	// the server file to find the DSN.
	var serverConfigPath string
	cmd.PersistentFlags().StringVarP(&serverConfigPath, "server-config", "s",
		"config/server.yml", "path to the server configuration file")
	var password string
	var passwordStdin bool

	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a UI account.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var pw string
			switch {
			case passwordStdin:
				pw = readPasswordStdin()
			case password != "":
				pw = password
			default:
				return errors.New("provide the password with --password or --password-stdin")
			}
			if pw == "" {
				return errors.New("the password must not be empty")
			}
			hash, err := auth.HashPassword(pw)
			if err != nil {
				return err
			}
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			if err := d.Users().CreateUser(context.Background(), args[0], hash); err != nil {
				return err
			}
			fmt.Printf("created user %q\n", args[0])
			return nil
		},
	}
	add.Flags().StringVar(&password, "password", "", "the account password (or use --password-stdin)")
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from a line of stdin")

	list := &cobra.Command{
		Use:   "list",
		Short: "List UI accounts.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			all, err := d.Users().ListUsers(context.Background())
			if err != nil {
				return err
			}
			for _, u := range all {
				fmt.Println(u.Name)
			}
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a UI account.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			if err := d.Users().RemoveUser(context.Background(), args[0]); err != nil {
				if errors.Is(err, auth.ErrNotFound) {
					return fmt.Errorf("user %q not found", args[0])
				}
				return err
			}
			fmt.Printf("removed user %q\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(add, list, remove)
	return cmd
}

// openDB opens the configured server database.
func openDB(serverConfigPath string) (*model.DB, error) {
	conf, err := cfg.LoadServer(serverConfigPath)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	d, err := model.Open(context.Background(), conf.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return d, nil
}

// readPasswordStdin reads one line from stdin and strips the trailing
// newline (and carriage return).
func readPasswordStdin() string {
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return ""
	}
	return strings.TrimRight(sc.Text(), "\r\n")
}

// requireUser fails with a friendly message when user does not exist.
func requireUser(d *model.DB, ctx context.Context, user string) error {
	if _, err := d.Users().GetUser(ctx, user); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			return fmt.Errorf("user %q not found", user)
		}
		return err
	}
	return nil
}

// expiryText renders a token expiry for the CLI output.
func expiryText(e *time.Time) string {
	if e == nil {
		return "never expires"
	}
	return "expires " + e.Format("2006-01-02 15:04:05")
}

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens for user-linked CLIs and integrations (stored in the database).",
	}
	// The tokens live in the configured database, so the subcommands need
	// the server file to find the DSN.
	var serverConfigPath string
	cmd.PersistentFlags().StringVarP(&serverConfigPath, "server-config", "s",
		"config/server.yml", "path to the server configuration file")

	var user string
	var org string
	var collection string
	var ttl time.Duration

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API token scoped to an org and collection; the raw value is shown exactly once.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if user == "" {
				return errors.New("provide the owning user with --user")
			}
			if org == "" {
				return errors.New("provide the org with --org")
			}
			if collection == "" {
				return errors.New("provide the collection with --collection")
			}
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			ctx := context.Background()
			if err := requireUser(d, ctx, user); err != nil {
				return err
			}
			raw, tok, err := d.Tokens().Create(ctx, user, org, collection, args[0], ttl)
			if err != nil {
				return err
			}
			// The raw value goes to stdout (so it can be captured with
			// $()); everything else goes to stderr.
			fmt.Fprintln(cmd.OutOrStdout(), raw)
			fmt.Fprintf(cmd.ErrOrStderr(), "created token %q for user %s in %s/%s (%s)\n",
				tok.Name, user, org, collection, expiryText(tok.Expires))
			return nil
		},
	}
	create.Flags().StringVar(&user, "user", "", "the user the token belongs to (required)")
	create.Flags().StringVar(&org, "org", "", "the org the token is scoped to (required)")
	create.Flags().StringVar(&collection, "collection", "", "the collection the token is scoped to (required)")
	create.Flags().DurationVar(&ttl, "ttl", 0, "token lifetime (e.g. 72h); 0 means no expiry")

	list := &cobra.Command{
		Use:   "list",
		Short: "List API tokens, optionally filtered by user, org and collection.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			ctx := context.Background()
			owner := ""
			if user != "" {
				if err := requireUser(d, ctx, user); err != nil {
					return err
				}
				owner = user
			}
			toks, err := d.Tokens().List(ctx, owner, org, collection)
			if err != nil {
				return err
			}
			for _, t := range toks {
				expiry := expiryText(t.Expires)
				last := "never used"
				if t.LastUsedAt != nil {
					last = "last used " + t.LastUsedAt.Format("2006-01-02 15:04:05")
				}
				name := t.Owner
				if name == "" {
					name = owner
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					name, t.Org, t.Collection, t.Name, t.Prefix, expiry, last)
			}
			return nil
		},
	}
	list.Flags().StringVar(&user, "user", "", "only list this user's tokens")
	list.Flags().StringVar(&org, "org", "", "only list tokens for this org")
	list.Flags().StringVar(&collection, "collection", "", "only list tokens for this collection")

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Revoke an API token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if user == "" {
				return errors.New("provide the owning user with --user")
			}
			if org == "" || collection == "" {
				return errors.New("provide the token's scope with --org and --collection")
			}
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			if err := d.Tokens().Remove(context.Background(), user, org, collection, args[0]); err != nil {
				if errors.Is(err, auth.ErrNotFound) {
					return fmt.Errorf("token %q not found for user %q in %s/%s", args[0], user, org, collection)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed token %q\n", args[0])
			return nil
		},
	}
	remove.Flags().StringVar(&user, "user", "", "the user that owns the token (required)")
	remove.Flags().StringVar(&org, "org", "", "the org the token is scoped to (required)")
	remove.Flags().StringVar(&collection, "collection", "", "the collection the token is scoped to (required)")

	cmd.AddCommand(create, list, remove)
	return cmd
}

// collectionCmd manages user-to-collection memberships. Once the first
// membership exists, only members can see and use their collections;
// until then every authenticated user has access (see auth/access.go).
func collectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collection",
		Short: "Manage user access to org/collection pairs.",
	}
	var serverConfigPath string
	cmd.PersistentFlags().StringVarP(&serverConfigPath, "server-config", "s",
		"config/server.yml", "path to the server configuration file")

	accessCmd := func(use, short, verb string, act func(d *model.DB, ctx context.Context, user, org, collection string) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(3),
			RunE: func(cmd *cobra.Command, args []string) error {
				d, err := openDB(serverConfigPath)
				if err != nil {
					return err
				}
				if err := act(d, context.Background(), args[0], args[1], args[2]); err != nil {
					return err
				}
				fmt.Printf("%s %s access to %s/%s\n", verb, args[0], args[1], args[2])
				return nil
			},
		}
	}

	grant := accessCmd("grant <user> <org> <collection>", "Allow a user to use one collection.", "granted",
		func(d *model.DB, ctx context.Context, user, org, collection string) error {
			return d.Access().Add(ctx, user, org, collection)
		})
	revoke := accessCmd("revoke <user> <org> <collection>", "Remove a user's access to one collection.", "revoked",
		func(d *model.DB, ctx context.Context, user, org, collection string) error {
			return d.Access().Remove(ctx, user, org, collection)
		})

	var user string
	list := &cobra.Command{
		Use:   "list",
		Short: "List collection memberships, for one user or all users.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB(serverConfigPath)
			if err != nil {
				return err
			}
			items, err := d.Access().List(context.Background(), user)
			if err != nil {
				return err
			}
			for _, m := range items {
				fmt.Printf("%s\t%s/%s\n", m.Username, m.Org, m.Collection)
			}
			return nil
		},
	}
	list.Flags().StringVar(&user, "user", "", "only list this user's memberships")

	cmd.AddCommand(grant, revoke, list)
	return cmd
}
