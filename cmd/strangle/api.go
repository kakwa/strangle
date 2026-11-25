package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/spf13/cobra"
)

// apiResponseLimit keeps a malformed or hostile server from streaming an
// unbounded body into a CLI process.
const apiResponseLimit = 8 << 20

// apiOptions holds the flags shared by the API-backed commands.
type apiOptions struct {
	url      string
	token    string
	org      string
	target   string
	json     bool
	limit    int
	offset   int
	playbook string
	host     string
	from     string
	to       string
	check    bool
}

type apiClient struct {
	base  string
	token string
	http  *http.Client
}

type apiScope struct {
	org        string
	collection string
}

// apiCommands returns the CLI subcommands backed by the versioned HTTP API.
func apiCommands() []*cobra.Command {
	return []*cobra.Command{runCmd(), runsCmd(), hostsCmd(), hostCmd(), hostkeysCmd()}
}

func runCmd() *cobra.Command {
	opts := &apiOptions{}
	cmd := &cobra.Command{
		Use:   "run <target> <playbook>",
		Short: "Queue a playbook run through the API.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient(*opts)
			if err != nil {
				return err
			}
			scope, err := client.resolveScope(args[0], opts.org)
			if err != nil {
				return err
			}
			body := map[string]any{
				"target":   scope.collection,
				"playbook": args[1],
				"check":    opts.check,
			}
			var out map[string]any
			if err := client.do(cmd.Context(), http.MethodPost, scopePath(scope, "run"), body, &out); err != nil {
				return err
			}
			if opts.json {
				return writeAPIJSON(cmd, out)
			}
			tsv(cmd, out["status"], out["run_id"], scope.org+"/"+scope.collection, out["playbook"], out["check"])
			return nil
		},
	}
	addAPIFlags(cmd, opts)
	cmd.Flags().BoolVar(&opts.check, "check", false, "run in check (dry-run) mode")
	return cmd
}

func runsCmd() *cobra.Command {
	opts := &apiOptions{}
	cmd := &cobra.Command{
		Use:   "runs [target]",
		Short: "List runs of a collection.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := commandTarget(args, *opts)
			if err != nil {
				return err
			}
			client, err := newAPIClient(*opts)
			if err != nil {
				return err
			}
			scope, err := client.resolveScope(target, opts.org)
			if err != nil {
				return err
			}
			q := url.Values{}
			for name, value := range map[string]string{
				"playbook": opts.playbook, "host": opts.host,
				"from": opts.from, "to": opts.to,
				"limit": fmt.Sprint(opts.limit), "offset": fmt.Sprint(opts.offset),
			} {
				if value != "" && value != "0" {
					q.Set(name, value)
				}
			}
			path := scopePath(scope, "runs")
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var runs []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, path, nil, &runs); err != nil {
				return err
			}
			if opts.json {
				return writeAPIJSON(cmd, runs)
			}
			for _, run := range runs {
				tsv(cmd, run["id"], run["target"], run["playbook"], run["started_at"],
					run["state"], run["success"], run["duration_ms"], run["hosts"],
					run["changed_hosts"], run["failed_hosts"])
			}
			return nil
		},
	}
	addAPIFlags(cmd, opts)
	cmd.Flags().StringVar(&opts.playbook, "playbook", "", "only runs of this playbook")
	cmd.Flags().StringVar(&opts.host, "host", "", "only runs that mention this host")
	cmd.Flags().StringVar(&opts.from, "from", "", "inclusive RFC3339 start time")
	cmd.Flags().StringVar(&opts.to, "to", "", "exclusive RFC3339 end time")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of runs")
	cmd.Flags().IntVar(&opts.offset, "offset", 0, "number of matching runs to skip")
	return cmd
}

func hostsCmd() *cobra.Command {
	opts := &apiOptions{}
	cmd := &cobra.Command{
		Use:   "hosts [target]",
		Short: "List the known hosts of a collection.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := commandTarget(args, *opts)
			if err != nil {
				return err
			}
			client, err := newAPIClient(*opts)
			if err != nil {
				return err
			}
			scope, err := client.resolveScope(target, opts.org)
			if err != nil {
				return err
			}
			var hosts []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, scopePath(scope, "hosts"), nil, &hosts); err != nil {
				return err
			}
			if opts.json {
				return writeAPIJSON(cmd, hosts)
			}
			for _, host := range hosts {
				tsv(cmd, host["name"], host["ip"], host["last_status"], host["in_inventory"],
					host["active"], host["last_seen_at"], host["last_run_at"])
			}
			return nil
		},
	}
	addAPIFlags(cmd, opts)
	return cmd
}

func hostCmd() *cobra.Command {
	opts := &apiOptions{}
	cmd := &cobra.Command{
		Use:   "host <name> [target]",
		Short: "Show one host, its run history and its latest changes.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := opts.target
			if len(args) > 1 {
				if target != "" && target != args[1] {
					return fmt.Errorf("target %q from --target conflicts with %q", target, args[1])
				}
				target = args[1]
			}
			if target == "" {
				return errors.New("target is required (second argument or --target)")
			}
			name := args[0]
			client, err := newAPIClient(*opts)
			if err != nil {
				return err
			}
			scope, err := client.resolveScope(target, opts.org)
			if err != nil {
				return err
			}

			var hosts []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, scopePath(scope, "hosts"), nil, &hosts); err != nil {
				return err
			}
			var host map[string]any
			for _, candidate := range hosts {
				if candidate["name"] == name {
					host = candidate
					break
				}
			}
			if host == nil {
				return fmt.Errorf("unknown host %q in %s/%s", name, scope.org, scope.collection)
			}

			runsPath := scopePath(scope, "hosts", name, "runs")
			changesPath := scopePath(scope, "hosts", name, "changes")
			if opts.limit > 0 {
				runsPath += "?limit=" + fmt.Sprint(opts.limit)
				changesPath += "?limit=" + fmt.Sprint(opts.limit)
			}
			var runs []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, runsPath, nil, &runs); err != nil {
				return err
			}
			var changes []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, changesPath, nil, &changes); err != nil {
				return err
			}
			if opts.json {
				return writeAPIJSON(cmd, map[string]any{"host": host, "runs": runs, "changes": changes})
			}
			tsv(cmd, "host", host["name"], host["ip"], host["last_status"], host["active"], host["last_run_at"])
			for _, run := range runs {
				tsv(cmd, "run", run["id"], run["started_at"], run["playbook"], run["success"], run["exit_code"])
			}
			for _, change := range changes {
				tsv(cmd, "change", change["task"], change["status"], change["last_run_at"], change["last_changed_at"])
			}
			return nil
		},
	}
	addAPIFlags(cmd, opts)
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of runs and changes")
	return cmd
}

func hostkeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hostkeys",
		Short: "List and decide pending SSH host keys.",
	}
	listOpts := &apiOptions{}
	list := &cobra.Command{
		Use:   "list [target]",
		Short: "List pending SSH host keys of a collection.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := commandTarget(args, *listOpts)
			if err != nil {
				return err
			}
			client, err := newAPIClient(*listOpts)
			if err != nil {
				return err
			}
			scope, err := client.resolveScope(target, listOpts.org)
			if err != nil {
				return err
			}
			var keys []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, scopePath(scope, "hostkeys"), nil, &keys); err != nil {
				return err
			}
			if listOpts.json {
				return writeAPIJSON(cmd, keys)
			}
			for _, key := range keys {
				tsv(cmd, key["target"], key["host"], key["fingerprint"], key["status"], key["first_seen"])
			}
			return nil
		},
	}
	addAPIFlags(list, listOpts)

	acceptOpts := &apiOptions{}
	accept := &cobra.Command{
		Use:   "accept <target/playbook> <host> <fingerprint>",
		Short: "Approve a pending SSH host key.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideHostKey(cmd, *acceptOpts, "accept", args)
		},
	}
	addAPIFlags(accept, acceptOpts)

	rejectOpts := &apiOptions{}
	reject := &cobra.Command{
		Use:   "reject <target/playbook> <host>",
		Short: "Reject a pending SSH host key.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideHostKey(cmd, *rejectOpts, "reject", args)
		},
	}
	addAPIFlags(reject, rejectOpts)

	cmd.AddCommand(list, accept, reject)
	return cmd
}

func decideHostKey(cmd *cobra.Command, opts apiOptions, decision string, args []string) error {
	sshScope, host := args[0], args[1]
	target, _, ok := strings.Cut(sshScope, "/")
	if !ok || target == "" {
		return fmt.Errorf("SSH scope %q must be <target>/<playbook>", sshScope)
	}
	if opts.target != "" && opts.target != target {
		return fmt.Errorf("target %q from --target conflicts with scope %q", opts.target, sshScope)
	}
	client, err := newAPIClient(opts)
	if err != nil {
		return err
	}
	scope, err := client.resolveScope(target, opts.org)
	if err != nil {
		return err
	}
	body := map[string]any{"target": sshScope, "host": host}
	if decision == "accept" {
		body["fingerprint"] = args[2]
	}
	var out map[string]any
	if err := client.do(cmd.Context(), http.MethodPost, scopePath(scope, "hostkeys", decision), body, &out); err != nil {
		return err
	}
	if opts.json {
		return writeAPIJSON(cmd, out)
	}
	tsv(cmd, out["status"], out["host"])
	return nil
}

func addAPIFlags(cmd *cobra.Command, opts *apiOptions) {
	cmd.Flags().StringVarP(&serverConfigPath, "server-config", "s", "config/server.yml", "path to the server configuration file")
	cmd.Flags().StringVarP(&collectionConfigPath, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")
	cmd.Flags().StringVar(&opts.url, "url", "", "API base URL (default derived from server.yml)")
	cmd.Flags().StringVar(&opts.token, "token", "", "user API token (default auth.cli_token from server.yml)")
	cmd.Flags().StringVar(&opts.org, "org", "", "org of the target (default from collection.yml)")
	cmd.Flags().StringVar(&opts.target, "target", "", "collection target name")
	cmd.Flags().BoolVar(&opts.json, "json", false, "print the API response as JSON")
}

func commandTarget(args []string, opts apiOptions) (string, error) {
	target := opts.target
	if len(args) > 0 {
		if target != "" && target != args[0] {
			return "", fmt.Errorf("target %q from --target conflicts with %q", target, args[0])
		}
		target = args[0]
	}
	if target == "" {
		return "", errors.New("target is required (argument or --target)")
	}
	return target, nil
}

func newAPIClient(opts apiOptions) (*apiClient, error) {
	cfg, err := config.LoadServer(serverConfigPath)
	if err != nil {
		return nil, err
	}
	base := opts.url
	if base == "" {
		base = apiBaseURL(cfg)
	}
	token := opts.token
	if token == "" {
		token = cfg.Auth.CLIToken
	}
	return &apiClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// apiBaseURL turns a listen address into a URL a CLI can dial. Wildcard bind
// addresses map to localhost, and TLS settings select HTTPS.
func apiBaseURL(cfg config.ServerConfig) string {
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "80"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	scheme := "http"
	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// resolveScope finds the org of a target from collection.yml. --org is an
// explicit alternative for remote use or stale local config.
func (c *apiClient) resolveScope(target, orgOverride string) (apiScope, error) {
	if target == "" {
		return apiScope{}, errors.New("target is required")
	}
	if orgOverride != "" {
		return apiScope{org: orgOverride, collection: target}, nil
	}
	specs, err := config.LoadCollection(collectionConfigPath)
	if err != nil {
		return apiScope{}, fmt.Errorf("resolve org of target %q: %w (pass --org or --collection-config)", target, err)
	}
	for _, spec := range specs {
		if spec.Name == target {
			return apiScope{org: spec.Org, collection: target}, nil
		}
	}
	return apiScope{}, fmt.Errorf("unknown target %q in %q (pass --org to set its org explicitly)", target, collectionConfigPath)
}

// scopePath builds a canonical collection-scoped API path. Every segment is
// escaped independently so host names and orgs cannot alter the route.
func scopePath(scope apiScope, parts ...string) string {
	path := "/api/v1/orgs/" + url.PathEscape(scope.org) + "/collections/" + url.PathEscape(scope.collection)
	for _, part := range parts {
		path += "/" + url.PathEscape(part)
	}
	return path
}

func (c *apiClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request for %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, apiResponseLimit))
	if err != nil {
		return fmt.Errorf("read API response from %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &apiErr)
		message := apiErr.Error
		if message == "" {
			message = strings.TrimSpace(string(payload))
		}
		if resp.StatusCode == http.StatusUnauthorized && c.token == "" {
			message += " (set auth.cli_token in server.yml or pass --token)"
		}
		return fmt.Errorf("API %s %s: %s: %s", method, path, resp.Status, message)
	}
	if out == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode API response from %s: %w", path, err)
	}
	return nil
}

func tsv(cmd *cobra.Command, fields ...any) {
	values := make([]string, len(fields))
	for i, field := range fields {
		if field == nil {
			values[i] = ""
			continue
		}
		values[i] = fmt.Sprint(field)
	}
	fmt.Fprintln(cmd.OutOrStdout(), strings.Join(values, "\t"))
}

func writeAPIJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
