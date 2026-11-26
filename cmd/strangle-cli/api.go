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

	"github.com/kakwa/strangle/cfg"
	"github.com/spf13/cobra"
)

// apiResponseLimit keeps a malformed or hostile server from streaming an
// unbounded body into a CLI process.
const apiResponseLimit = 8 << 20

// apiOptions holds the flags shared by the API-backed commands.
type apiOptions struct {
	serverConfig     string
	collectionConfig string
	url              string
	token            string
	org              string
	target           string
	json             bool
	limit            int
	playbook         string
	host             string
	fingerprint      string
}

type apiClient struct {
	base             string
	token            string
	collectionConfig string
	http             *http.Client
}

type apiScope struct {
	org        string
	collection string
}

// apiCommands returns the CLI subcommands backed by the versioned HTTP API.
func apiCommands() []*cobra.Command {
	return []*cobra.Command{hostsCmd(), hostCmd(), hostkeysCmd()}
}

func hostsCmd() *cobra.Command {
	opts := &apiOptions{}
	cmd := &cobra.Command{
		Use:   "hosts [target]",
		Short: "List the known hosts of a collection.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, scope, err := clientFor(opts, args, 0)
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
		Short: "Show one host and its run history for a playbook.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.playbook == "" {
				return errors.New("a playbook is required (--playbook)")
			}
			name := args[0]
			client, scope, err := clientFor(opts, args, 1)
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

			runsPath := scopePath(scope, "playbooks", opts.playbook, "hosts", name, "runs")
			if opts.limit > 0 {
				runsPath += "?limit=" + fmt.Sprint(opts.limit)
			}
			var runs []map[string]any
			if err := client.do(cmd.Context(), http.MethodGet, runsPath, nil, &runs); err != nil {
				return err
			}
			if opts.json {
				return writeAPIJSON(cmd, map[string]any{"host": host, "runs": runs})
			}
			tsv(cmd, "host", host["name"], host["ip"], host["last_status"], host["active"], host["last_run_at"])
			for _, run := range runs {
				tsv(cmd, "run", run["id"], run["started_at"], run["playbook"], run["success"], run["exit_code"])
			}
			return nil
		},
	}
	addAPIFlags(cmd, opts)
	cmd.Flags().StringVar(&opts.playbook, "playbook", "", "playbook whose host history to show (required)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of runs")
	_ = cmd.MarkFlagRequired("playbook")
	return cmd
}

func hostkeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hostkeys",
		Short: "List and decide SSH host keys.",
	}
	listOpts := &apiOptions{}
	list := &cobra.Command{
		Use:   "list [target] [host]",
		Short: "List the SSH host keys of one host in a playbook.",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetArg, err := commandTarget(args, listOpts.target, "target", 0)
			if err != nil {
				return err
			}
			target, playbook, err := resolveTargetPlaybook(targetArg, listOpts.playbook)
			if err != nil {
				return err
			}
			host, err := commandTarget(args, listOpts.host, "host", 1)
			if err != nil {
				return err
			}
			client, scope, err := scopedClient(listOpts, target)
			if err != nil {
				return err
			}
			var keys []map[string]any
			path := scopePath(scope, "playbooks", playbook, "hosts", host, "hostkeys")
			if err := client.do(cmd.Context(), http.MethodGet, path, nil, &keys); err != nil {
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
	list.Flags().StringVar(&listOpts.playbook, "playbook", "", "playbook that contains the host (required)")
	list.Flags().StringVar(&listOpts.host, "host", "", "host whose keys to list (or second argument)")
	_ = list.MarkFlagRequired("playbook")

	approveOpts := &apiOptions{}
	approve := &cobra.Command{
		Use:   "approve <target/playbook> <host> [fingerprint]",
		Short: "Approve a pending SSH host key.",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideHostKey(cmd, *approveOpts, "approved", args)
		},
	}
	addAPIFlags(approve, approveOpts)
	approve.Flags().StringVar(&approveOpts.playbook, "playbook", "", "playbook that contains the host (or use <target/playbook>)")
	approve.Flags().StringVar(&approveOpts.fingerprint, "fingerprint", "", "fingerprint to record (or use the third argument)")

	rejectOpts := &apiOptions{}
	reject := &cobra.Command{
		Use:   "reject <target/playbook> <host>",
		Short: "Reject a pending SSH host key.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decideHostKey(cmd, *rejectOpts, "rejected", args)
		},
	}
	addAPIFlags(reject, rejectOpts)
	reject.Flags().StringVar(&rejectOpts.playbook, "playbook", "", "playbook that contains the host (or use <target/playbook>)")

	cmd.AddCommand(list, approve, reject)
	return cmd
}

// resolveTargetPlaybook accepts the legacy "<target>/<playbook>" spelling and
// the newer "target plus --playbook" spelling. Both are kept because they are
// the user-facing names of the scope, not details of the HTTP route.
func resolveTargetPlaybook(targetArg, playbookFlag string) (string, string, error) {
	if targetArg == "" {
		return "", "", errors.New("target is required")
	}
	target, playbook, hasPlaybook := strings.Cut(targetArg, "/")
	if hasPlaybook {
		if target == "" || playbook == "" {
			return "", "", fmt.Errorf("target %q must be <target>/<playbook>", targetArg)
		}
		if playbookFlag != "" && playbookFlag != playbook {
			return "", "", fmt.Errorf("playbook %q from --playbook conflicts with scope %q", playbookFlag, targetArg)
		}
	} else {
		target = targetArg
		playbook = playbookFlag
	}
	if playbook == "" {
		return "", "", errors.New("a playbook is required (--playbook or <target/playbook>)")
	}
	return target, playbook, nil
}

func decideHostKey(cmd *cobra.Command, opts apiOptions, status string, args []string) error {
	target, playbook, err := resolveTargetPlaybook(args[0], opts.playbook)
	if err != nil {
		return err
	}
	host := args[1]
	if opts.target != "" && opts.target != target {
		return fmt.Errorf("target %q from --target conflicts with %q", target, opts.target)
	}
	client, scope, err := scopedClient(&opts, target)
	if err != nil {
		return err
	}

	fingerprint := opts.fingerprint
	if len(args) > 2 && args[2] != "" {
		if fingerprint != "" && fingerprint != args[2] {
			return fmt.Errorf("fingerprint %q from --fingerprint conflicts with %q", fingerprint, args[2])
		}
		fingerprint = args[2]
	}
	ops := []map[string]any{{"op": "replace", "path": "/status", "value": status}}
	if fingerprint != "" {
		if status != "approved" {
			return errors.New("a fingerprint can only be given when approving")
		}
		ops = append(ops, map[string]any{"op": "replace", "path": "/fingerprint", "value": fingerprint})
	}
	var out map[string]any
	path := scopePath(scope, "playbooks", playbook, "hosts", host)
	if err := client.do(cmd.Context(), http.MethodPatch, path, ops, &out); err != nil {
		return err
	}
	if opts.json {
		return writeAPIJSON(cmd, out)
	}
	value := out["status"]
	if value == nil {
		value = status
	}
	keyHost := out["host"]
	if keyHost == nil {
		keyHost = host
	}
	tsv(cmd, value, keyHost)
	return nil
}

func addAPIFlags(cmd *cobra.Command, opts *apiOptions) {
	cmd.Flags().StringVarP(&opts.serverConfig, "server-config", "s", "config/server.yml", "path to the server configuration file")
	cmd.Flags().StringVarP(&opts.collectionConfig, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")
	cmd.Flags().StringVar(&opts.url, "url", "", "API base URL (default derived from server.yml)")
	cmd.Flags().StringVar(&opts.token, "token", "", "user API token (default auth.cli_token from server.yml)")
	cmd.Flags().StringVar(&opts.org, "org", "", "org of the target (default from collection.yml)")
	cmd.Flags().StringVar(&opts.target, "target", "", "collection target name")
	cmd.Flags().BoolVar(&opts.json, "json", false, "print the API response as JSON")
}

// commandTarget resolves a required value from a positional argument (at
// index) or a flag, erroring when both are given and disagree.
func commandTarget(args []string, flag, name string, index int) (string, error) {
	value := flag
	if len(args) > index && args[index] != "" {
		if value != "" && value != args[index] {
			return "", fmt.Errorf("%s %q from --%s conflicts with %q", name, value, name, args[index])
		}
		value = args[index]
	}
	if value == "" {
		return "", fmt.Errorf("%s is required (argument or --%s)", name, name)
	}
	return value, nil
}

// scopedClient builds the API client and resolves the scope of target.
func scopedClient(opts *apiOptions, target string) (*apiClient, apiScope, error) {
	client, err := newAPIClient(*opts)
	if err != nil {
		return nil, apiScope{}, err
	}
	scope, err := client.resolveScope(target, opts.org)
	if err != nil {
		return nil, apiScope{}, err
	}
	return client, scope, nil
}

// clientFor resolves the target from a positional argument (at index) or the
// --target flag, then builds the client and its scope.
func clientFor(opts *apiOptions, args []string, index int) (*apiClient, apiScope, error) {
	target, err := commandTarget(args, opts.target, "target", index)
	if err != nil {
		return nil, apiScope{}, err
	}
	return scopedClient(opts, target)
}

func newAPIClient(opts apiOptions) (*apiClient, error) {
	conf, err := cfg.LoadServer(opts.serverConfig)
	if err != nil {
		return nil, err
	}
	base := opts.url
	if base == "" {
		base = apiBaseURL(conf)
	}
	token := opts.token
	if token == "" {
		token = conf.Auth.CLIToken
	}
	return &apiClient{
		base:             strings.TrimRight(base, "/"),
		token:            token,
		collectionConfig: opts.collectionConfig,
		http:             &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// apiBaseURL turns a listen address into a URL a CLI can dial. Wildcard bind
// addresses map to localhost, and TLS settings select HTTPS.
func apiBaseURL(conf cfg.ServerConfig) string {
	addr := conf.ListenAddr
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
	if conf.TLS.CertFile != "" && conf.TLS.KeyFile != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// resolveScope finds the org of a target from collection.yml. --org is an
// explicit alternative for remote use or stale local cfg.
func (c *apiClient) resolveScope(target, orgOverride string) (apiScope, error) {
	if target == "" {
		return apiScope{}, errors.New("target is required")
	}
	if orgOverride != "" {
		return apiScope{org: orgOverride, collection: target}, nil
	}
	specs, err := cfg.LoadCollection(c.collectionConfig)
	if err != nil {
		return apiScope{}, fmt.Errorf("resolve org of target %q: %w (pass --org or --collection-config)", target, err)
	}
	for _, spec := range specs {
		if spec.Name == target {
			return apiScope{org: spec.Org, collection: target}, nil
		}
	}
	return apiScope{}, fmt.Errorf("unknown target %q in %q (pass --org to set its org explicitly)", target, c.collectionConfig)
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
