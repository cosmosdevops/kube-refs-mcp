// Package cli implements the kube-refs-mcp command line. It exists for
// local testing, debugging and CI; it is not a deployment tool.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosmosdevops/kube-refs-mcp/internal/app"
	"github.com/cosmosdevops/kube-refs-mcp/internal/checks"
	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
	"github.com/cosmosdevops/kube-refs-mcp/internal/manifests"
	"github.com/cosmosdevops/kube-refs-mcp/internal/mcpserver"
)

// Exit codes of the check command.
const (
	ExitConsistent = 0
	ExitBroken     = 1
	ExitIncomplete = 2
	ExitError      = 3
)

// exitError carries a specific process exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Main runs the CLI and returns the process exit code.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRoot(stdin)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(stderr, ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(stderr, "error:", err)
	return ExitError
}

type globals struct {
	opts app.Options
}

func (g *globals) app() (*app.App, error) { return app.New(g.opts) }

func newRoot(stdin io.Reader) *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "kube-refs-mcp",
		Short:         "Check that a bundle of Kubernetes manifests fits together, offline",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringArrayVar(&g.opts.WorkspaceRoots, "workspace-root", nil, "directory the MCP server may read manifests from (repeatable)")
	pf.Int64Var(&g.opts.MaxManifestBytes, "max-manifest-bytes", app.DefaultMaxManifestBytes, "largest manifest input accepted, in bytes")
	pf.IntVar(&g.opts.MaxResources, "max-resources", app.DefaultMaxResources, "most resources accepted in one check")
	pf.IntVar(&g.opts.MaxFiles, "max-files", app.DefaultMaxFiles, "most manifest files read from one directory")
	pf.DurationVar(&g.opts.Timeout, "timeout", app.DefaultTimeout, "longest one check may run")
	root.AddCommand(serveCmd(g), newKeyCmd(), checkHealthCmd(), checkCmd(g, stdin), linksCmd(g, stdin), checksCmd(), versionCmd())
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		fmt.Fprintln(cmd.OutOrStdout(), app.Version)
		return nil
	}}
}

// checksCmd lists the checks in this build.
func checksCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{Use: "checks", Short: "List the checks and what each one decides", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		switch output {
		case "json":
			return writeJSON(out, checks.List())
		case "text":
			for _, c := range checks.List() {
				fmt.Fprintf(out, "%s\n  %s\n  %s\n  kinds: %s\n", c.Name, c.Description, c.Decides, strings.Join(c.Kinds, ", "))
			}
			return nil
		}
		return fmt.Errorf("unknown output format %q (use text or json)", output)
	}}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}

func serveCmd(g *globals) *cobra.Command {
	var (
		addr, keysFile string
		perMinute      float64
		burst          int
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server on stdio, or over HTTP with --http",
		Long: `Run the MCP server.

With no flags it talks MCP over stdin/stdout, for an agent to run locally.

With --http it serves MCP over HTTP at /mcp, for hosting. Callers need an API
key from the --keys file, and each key has its own rate limit. Make keys with
the new-key command. /healthz is open and needs no key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.app()
			if err != nil {
				return err
			}
			about := "workspace roots: " + strings.Join(a.Workspace.Roots(), ", ")
			if len(a.Workspace.Roots()) == 0 {
				about = "no workspace roots, so manifests are sent inline"
			}
			if off := a.SkippedChecks(); len(off) > 0 {
				about += "; checks turned off: " + strings.Join(off, ", ")
			}
			if addr == "" {
				if keysFile != "" {
					return errors.New("--keys only applies with --http")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "kube-refs-mcp %s: serving MCP on stdio (%s)\n", app.Version, about)
				return mcpserver.Run(cmd.Context(), a)
			}
			if keysFile == "" {
				return errors.New("--http needs --keys: the hosted server only answers callers with an API key")
			}
			log.SetFlags(0)
			log.SetOutput(cmd.ErrOrStderr())
			log.Printf("kube-refs-mcp %s: serving MCP on http://%s/mcp, %g requests a minute per key (%s)", app.Version, addr, perMinute, about)
			return mcpserver.RunHTTP(cmd.Context(), a, mcpserver.HTTPOptions{
				Addr: addr, KeysFile: keysFile, PerMinute: perMinute, Burst: burst, MaxInput: g.opts.MaxManifestBytes,
			})
		},
	}
	cmd.Flags().StringArrayVar(&g.opts.SkipChecks, "skip-check", nil, "turn this check off for every caller; * is a wildcard (repeatable). See the checks command")
	cmd.Flags().StringVar(&addr, "http", "", "serve MCP over HTTP on this address (e.g. 127.0.0.1:8089) instead of stdin/stdout; needs --keys")
	cmd.Flags().StringVar(&keysFile, "keys", "", "file of API keys for --http, one \"name sha256\" per line; add lines with the new-key command")
	cmd.Flags().Float64Var(&perMinute, "rate", 60, "with --http: requests each API key may make per minute")
	cmd.Flags().IntVar(&burst, "burst", 30, "with --http: requests each API key may make at once before the rate applies")
	return cmd
}

// newKeyCmd makes an API key for the hosted mode.
func newKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new-key NAME",
		Short: "Make an API key for serve --http",
		Long: `Make an API key for a named holder.

The key is printed once on stderr, for you to pass on. The line for the keys
file is printed on stdout, so it can be appended:

  kube-refs-mcp new-key alice >> /etc/kube-refs-mcp/keys

The keys file holds the name and the SHA-256 of each key, never the key
itself. To revoke a key, delete its line.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, line, err := mcpserver.NewKey(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "API key for %s (shown once, it is not stored):\n\n  %s\n\n", args[0], key)
			fmt.Fprintln(cmd.OutOrStdout(), line)
			return nil
		},
	}
}

// checkHealthCmd asks a running server whether it is up. Container images
// have no curl to do this with.
func checkHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-health URL",
		Short: "Check a running server's health URL; for container health checks",
		Long:  "Ask a running server's health URL (e.g. http://127.0.0.1:8089/healthz) and exit 0 if it is up.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return mcpserver.CheckHealth(args[0])
		},
	}
}

// input holds the flags that say what to check. The check and links
// commands share them.
type input struct {
	files        []string
	dir          string
	namespace    string
	complete     bool
	clusterFiles []string
	only, skip   []string
}

func (in *input) flags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringArrayVarP(&in.files, "file", "f", nil, "manifest file to check; use - for stdin (repeatable)")
	f.StringVar(&in.dir, "dir", "", "check every manifest under this directory")
	f.StringVarP(&in.namespace, "namespace", "n", "", "the namespace the bundle is deployed to, for resources that set none")
	f.BoolVar(&in.complete, "complete", false, "the bundle is everything that will exist: a missing target counts as broken")
	f.StringArrayVar(&in.clusterFiles, "cluster-objects", nil, "YAML or JSON file of objects that already exist in the cluster (repeatable)")
	f.StringArrayVar(&in.only, "check", nil, "only run this check; * is a wildcard, as in 'ingress-*' (repeatable). See the checks command")
	f.StringArrayVar(&in.skip, "skip-check", nil, "leave this check out; * is a wildcard (repeatable)")
}

// run loads the manifests and checks them.
func (in *input) run(ctx context.Context, a *app.App, stdin io.Reader, all bool) (*checks.Result, error) {
	if len(in.files) == 0 && in.dir == "" {
		return nil, errors.New("give at least one --file or a --dir")
	}
	lim := a.ManifestLimits()
	read := func(f string) ([]manifests.Resource, error) {
		var data []byte
		var err error
		name := f
		if f == "-" {
			data, err = io.ReadAll(io.LimitReader(stdin, lim.MaxBytes+1))
			name = "stdin"
		} else {
			// The person at the command line chose this file, so a pipe is
			// read too, as in -f <(helm template .). The read is still
			// bounded.
			var file *os.File
			if file, err = os.Open(f); err == nil {
				data, err = io.ReadAll(io.LimitReader(file, lim.MaxBytes+1))
				file.Close()
			}
		}
		if err != nil {
			return nil, err
		}
		return manifests.Parse(data, name, lim)
	}
	req := checks.Request{Namespace: in.namespace, BundleComplete: in.complete, IncludeAll: all, Checks: in.only, SkipChecks: in.skip}
	for _, f := range in.files {
		rs, err := read(f)
		if err != nil {
			return nil, err
		}
		req.Resources = append(req.Resources, rs...)
	}
	if in.dir != "" {
		// The operator chose this directory, so it is the workspace.
		ws, err := manifests.NewWorkspace([]string{in.dir})
		if err != nil {
			return nil, err
		}
		rs, sk, err := ws.LoadDir(".", a.DirLimits())
		if err != nil {
			return nil, err
		}
		for i := range rs {
			rs[i].Source = filepath.Join(in.dir, rs[i].Source)
			rs[i].File = filepath.Join(in.dir, rs[i].File)
		}
		for i := range sk {
			sk[i].Path = filepath.Join(in.dir, sk[i].Path)
		}
		req.Resources = append(req.Resources, rs...)
		req.SkippedFiles = sk
	}
	if len(req.Resources) == 0 {
		return nil, errors.New("no Kubernetes manifests found")
	}
	for _, f := range in.clusterFiles {
		rs, err := read(f)
		if err != nil {
			return nil, fmt.Errorf("cluster objects: %w", err)
		}
		req.ClusterObjects = append(req.ClusterObjects, rs...)
	}
	return a.Check(ctx, req)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// exitFor maps a report to the exit code.
func exitFor(r *links.Report) error {
	switch {
	case r.Summary.Broken > 0:
		return &exitError{code: ExitBroken}
	case !r.Complete:
		return &exitError{code: ExitIncomplete}
	}
	return nil
}

func checkCmd(g *globals, stdin io.Reader) *cobra.Command {
	var (
		in      input
		output  string
		all     bool
		explain bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check that a bundle of manifests fits together",
		Long: `Check that a bundle of manifests fits together.

Give every manifest that is deployed together in one run. A reference can
only be followed to a resource that is part of the same run.

Exit codes: 0 consistent, 1 broken references, 2 nothing broken but some
references are unresolved or a check failed, 3 usage or runtime error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.app()
			if err != nil {
				return err
			}
			res, err := in.run(cmd.Context(), a, stdin, all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch output {
			case "json":
				var payload any = res.Report
				if explain {
					exps := []*checks.Explanation{}
					for _, f := range res.Report.Findings {
						if ex, err := res.Explain(f.ID); err == nil {
							exps = append(exps, ex)
						}
					}
					payload = map[string]any{"report": res.Report, "explanations": exps}
				}
				if err := writeJSON(out, payload); err != nil {
					return err
				}
			case "text":
				printReport(out, res, explain)
			default:
				return fmt.Errorf("unknown output format %q (use text or json)", output)
			}
			return exitFor(res.Report)
		},
	}
	in.flags(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&all, "all", false, "also list the links that are ok or skipped")
	cmd.Flags().BoolVar(&explain, "explain", false, "explain each finding")
	return cmd
}

func linksCmd(g *globals, stdin io.Reader) *cobra.Command {
	var (
		in     input
		output string
	)
	cmd := &cobra.Command{
		Use:   "links",
		Short: "List every reference between the resources of a bundle",
		Long: `List every reference between the resources of a bundle, including the
ones that are fine. This always exits 0 unless the bundle cannot be read;
use the check command for a verdict.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.app()
			if err != nil {
				return err
			}
			res, err := in.run(cmd.Context(), a, stdin, true)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch output {
			case "json":
				return writeJSON(out, mcpserver.GraphOf(res))
			case "text":
				printLinks(out, res)
				return nil
			}
			return fmt.Errorf("unknown output format %q (use text or json)", output)
		},
	}
	in.flags(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}

func printHeader(w io.Writer, r *links.Report) {
	fmt.Fprintf(w, "Verdict:   %s (complete: %t, consistent: %t)\n", strings.ToUpper(string(r.Status)), r.Complete, r.Consistent)
	s := r.Summary
	fmt.Fprintf(w, "Checked:   %d resource(s), %d link(s)\n", s.Resources, s.Links)
	fmt.Fprintf(w, "Results:   %d broken, %d unresolved, %d error(s), %d ok, %d skipped\n\n", s.Broken, s.Unresolved, s.Errors, s.OK, s.Skipped)
}

func printFooter(w io.Writer, r *links.Report) {
	for _, sk := range r.SkippedFiles {
		label := "skipped"
		if sk.NotRead {
			label = "NOT READ"
		}
		fmt.Fprintf(w, "%s %s: %s\n", label, sk.Path, sk.Reason)
	}
	if len(r.Notes) > 0 {
		fmt.Fprintln(w, "\nNotes:")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
}

func printReport(w io.Writer, res *checks.Result, explain bool) {
	r := res.Report
	printHeader(w, r)
	fileOf := map[string]string{}
	for _, mr := range res.Resources {
		fileOf[mr.Ref().Key()] = mr.File
	}
	for _, fg := range links.GroupBySource(r, func(ref links.Ref) string { return fileOf[ref.Key()] }) {
		fmt.Fprintf(w, "%s\n", fg.Path)
		for _, rg := range fg.Resources {
			if len(rg.Findings) == 0 {
				fmt.Fprintf(w, "  %s: no findings\n", rg.Resource)
				continue
			}
			fmt.Fprintf(w, "  %s\n", rg.Resource)
			for _, f := range rg.Findings {
				fmt.Fprintf(w, "    %-11s %s -> %s\n", strings.ToUpper(string(f.Status)), f.Check, f.To)
				fmt.Fprintf(w, "      %s\n", f.Message)
				if f.Field != "" {
					fmt.Fprintf(w, "      field: %s\n", f.Field)
				}
				if f.ClosestMatch != "" {
					fmt.Fprintf(w, "      closest match: %s\n", f.ClosestMatch)
				}
				if f.Fix != "" {
					fmt.Fprintf(w, "      fix: %s\n", f.Fix)
				}
				if explain {
					if ex, err := res.Explain(f.ID); err == nil {
						if ex.Expected != "" {
							fmt.Fprintf(w, "      expected: %s\n", ex.Expected)
						}
						if ex.Found != "" {
							fmt.Fprintf(w, "      found: %s\n", ex.Found)
						}
						for _, c := range ex.Candidates {
							fmt.Fprintf(w, "      exists: %s\n", c)
						}
					}
				}
			}
		}
	}
	printFooter(w, r)
}

func printLinks(w io.Writer, res *checks.Result) {
	r := res.Report
	printHeader(w, r)
	var from string
	for _, l := range res.Links {
		if l.From.String() != from {
			from = l.From.String()
			fmt.Fprintf(w, "%s\n", from)
		}
		fmt.Fprintf(w, "  %-11s %-22s -> %s\n", strings.ToUpper(string(l.Status)), l.Check, l.To)
	}
	if len(res.Links) == 0 {
		fmt.Fprintln(w, "No references between resources were found.")
	}
	printFooter(w, r)
}
