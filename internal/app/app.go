// Package app holds the settings shared by the CLI and the MCP server: the
// workspace, the input limits and the time limit. Both are thin layers over
// it and the checks package.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/cosmosdevops/kube-refs-mcp/internal/checks"
	"github.com/cosmosdevops/kube-refs-mcp/internal/manifests"
)

// Version is what the version command prints and what the server reports
// to clients. Nothing in the code states it: it comes from the git tag being
// built. See version.go.
var Version = resolveVersion(buildVersion, moduleVersion())

// Defaults for the limits.
const (
	DefaultMaxManifestBytes int64 = 4 << 20
	DefaultMaxResources           = 2000
	DefaultMaxFiles               = 2000
	DefaultTimeout                = 30 * time.Second
)

// Options are the settings an operator may change with flags.
type Options struct {
	// WorkspaceRoots are the directories files may be read from. With
	// none, callers send manifests inline.
	WorkspaceRoots   []string
	MaxManifestBytes int64
	MaxResources     int
	MaxFiles         int
	Timeout          time.Duration
	// SkipChecks names checks that are turned off for every request.
	SkipChecks []string
}

// App is the assembled application core.
type App struct {
	Workspace *manifests.Workspace
	opts      Options
}

// New checks the options and builds the application.
func New(opts Options) (*App, error) {
	if opts.MaxManifestBytes <= 0 || opts.MaxResources <= 0 || opts.MaxFiles <= 0 {
		return nil, errors.New("the size limits must be above 0")
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("the timeout must be above 0")
	}
	// A mistyped check name stops the program at start-up, not later.
	sel, err := checks.Select(nil, opts.SkipChecks)
	if err != nil {
		return nil, err
	}
	opts.SkipChecks = sel.Skipped()
	ws, err := manifests.NewWorkspace(opts.WorkspaceRoots)
	if err != nil {
		return nil, err
	}
	return &App{Workspace: ws, opts: opts}, nil
}

// ManifestLimits returns the manifest input limits.
func (a *App) ManifestLimits() manifests.Limits {
	return manifests.Limits{MaxBytes: a.opts.MaxManifestBytes, MaxResources: a.opts.MaxResources}
}

// SkippedChecks lists the checks the operator turned off.
func (a *App) SkippedChecks() []string { return append([]string{}, a.opts.SkipChecks...) }

// DirLimits returns the directory walk limits.
func (a *App) DirLimits() manifests.DirLimits {
	return manifests.DirLimits{Limits: a.ManifestLimits(), MaxFiles: a.opts.MaxFiles}
}

// Check runs the checks within the time limit.
func (a *App) Check(ctx context.Context, req checks.Request) (*checks.Result, error) {
	if len(req.Resources)+len(req.ClusterObjects) > a.opts.MaxResources {
		return nil, errors.New("too many resources in one request")
	}
	// What the operator turned off stays off, whatever the caller asks.
	req.SkipChecks = append(append([]string(nil), req.SkipChecks...), a.opts.SkipChecks...)
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	return checks.Run(ctx, req)
}
