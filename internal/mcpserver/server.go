// Package mcpserver exposes the checks as Model Context Protocol tools. It
// contains no checking logic of its own.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cosmosdevops/kube-refs-mcp/internal/app"
	"github.com/cosmosdevops/kube-refs-mcp/internal/checks"
	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
	"github.com/cosmosdevops/kube-refs-mcp/internal/manifests"
)

const instructions = `This server checks that a bundle of Kubernetes manifests fits together, without a cluster. It follows the references between resources: a Service to the pods it selects and their ports, a pod to its ConfigMaps, Secrets, claims and service account, an Ingress or Route to its Service, an autoscaler to its workload, a role binding to its role.

It does not check schemas and it does not check policies. Use it after a schema check and before a policy check.

Typical workflow:
1. check_manifests with all the YAML that is deployed together. Send the whole bundle in one call: a reference can only be followed to a resource that is in the same call.
2. Read "status", "complete" and "findings". Each finding names both ends of a link and says what is wrong. "closestMatch" names the nearest thing that does exist.
3. Fix the manifests and check again until "consistent" is true.

How to read results:
- "broken": certain. Both ends are in the bundle and they do not fit.
- "unresolved": the target is not in the bundle. It may exist in the cluster, so this is not a failure, and it is NOT a pass. If the object does exist in the cluster, pass it in clusterObjects. If the bundle is everything, set bundleComplete and a missing target counts as broken.
- status "unknown" or complete=false: nothing is broken, but some links are unresolved.
Every check runs unless you narrow it with "checks" or "skipChecks"; list_checks names them. A check that is turned off is listed in "checksSkipped", and the verdict then covers only the checks that ran.
describe_links returns every link, including the ones that are fine. The server never changes manifests and never contacts a cluster.`

// maxHistory is how many checks are kept for explain_finding, and
// maxHistoryLinks how many links those checks may hold between them. The
// newest check is always kept.
const (
	maxHistory      = 50
	maxHistoryLinks = 20000
)

// Server holds the MCP tool handlers.
type Server struct {
	app *app.App

	// Past checks are kept per caller for explain_finding. Over stdio there
	// is one caller, "". Over HTTP each API key holder has their own, so
	// one holder cannot read another's results.
	mu      sync.Mutex
	history map[string]*callerHistory
}

type callerHistory struct {
	results map[string]*checks.Result
	order   []string
	links   int
}

// caller names who made a request: the API key holder over HTTP, or "" over
// stdio.
func caller(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(holderHeader)
}

// New creates the MCP server with all tools registered.
func New(a *app.App) (*Server, *mcp.Server) {
	s := &Server{app: a, history: map[string]*callerHistory{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "kube-refs-mcp", Title: "Kubernetes Reference Checks", Version: app.Version}, &mcp.ServerOptions{
		Instructions: instructions,
		// The tool list never changes while the server runs. Saying so
		// stops clients from holding a connection open to wait for changes,
		// which over HTTP ties up a connection per client.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check_manifests",
		Description: "Check that a bundle of Kubernetes manifests fits together, offline. Pass inline YAML or JSON (multiple documents allowed) in 'manifests', or workspace file paths in 'files'. Send everything that is deployed together in one call. Returns the verdict and one finding per reference that is broken or could not be resolved, naming both ends and the closest match. Nothing is applied to a cluster and the manifests are not changed.",
		Annotations: readOnly,
	}, s.checkManifests)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check_directory",
		Description: "Check every manifest file (.yaml, .yml, .json) under a workspace directory as one bundle. Findings are grouped by file and resource. Files that are not Kubernetes manifests are listed in skippedFiles. A file that could not be read has notRead set there, and the report is then not complete.",
		Annotations: readOnly,
	}, s.checkDirectory)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "describe_links",
		Description: "Return every reference found between the resources of a bundle, including the ones that are fine: which Service routes to which workload, which pod uses which ConfigMap, and so on. Takes inline YAML, workspace files or a workspace directory. Use it to understand how a bundle is wired.",
		Annotations: readOnly,
	}, s.describeLinks)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_checks",
		Description: "List the checks this server can run: the name each finding carries in its 'check' field, what the check looks at, and which outcomes it can be certain of. Also says which checks the operator of this server has turned off. Use the names in the 'checks' and 'skipChecks' inputs of the other tools.",
		Annotations: readOnly,
	}, s.listChecks)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "explain_finding",
		Description: "Explain one finding from an earlier check_manifests, check_directory or describe_links call: what was expected, what was found, what else exists that it could have meant, and how to fix it.",
		Annotations: readOnly,
	}, s.explainFinding)
	return s, srv
}

func ptr[T any](v T) *T { return &v }

// ---- inputs shared by the check tools ----

type checkManifestsInput struct {
	Manifests      string           `json:"manifests,omitempty" jsonschema:"inline Kubernetes YAML or JSON; several documents may be separated by ---"`
	Files          []string         `json:"files,omitempty" jsonschema:"manifest file paths inside the workspace, relative to the workspace root"`
	Namespace      string           `json:"namespace,omitempty" jsonschema:"the namespace the bundle is deployed to; used for resources that set none"`
	BundleComplete bool             `json:"bundleComplete,omitempty" jsonschema:"set to true when the bundle, with clusterObjects, is everything that will exist. A missing target then counts as broken instead of unresolved"`
	ClusterObjects []map[string]any `json:"clusterObjects,omitempty" jsonschema:"Kubernetes objects that already exist in the cluster, as context. They can be the target of a reference but are not checked themselves. Include the parts a reference needs: the keys of a ConfigMap or Secret (values may be left empty), the ports of a Service, the pod template labels of a workload"`
	IncludeAll     bool             `json:"includeAll,omitempty" jsonschema:"also list the links that are ok or skipped; by default only links that need attention are listed"`
	Checks         []string         `json:"checks,omitempty" jsonschema:"only run these checks. An entry may use * as a wildcard, for example ingress-*. list_checks shows the names. Leave out to run every check"`
	SkipChecks     []string         `json:"skipChecks,omitempty" jsonschema:"leave these checks out. An entry may use * as a wildcard"`
}

type checkDirectoryInput struct {
	Directory      string           `json:"directory" jsonschema:"directory inside the workspace, relative to the workspace root; use . for the root"`
	Namespace      string           `json:"namespace,omitempty" jsonschema:"the namespace the bundle is deployed to; used for resources that set none"`
	BundleComplete bool             `json:"bundleComplete,omitempty" jsonschema:"set to true when the directory, with clusterObjects, is everything that will exist. A missing target then counts as broken instead of unresolved"`
	ClusterObjects []map[string]any `json:"clusterObjects,omitempty" jsonschema:"Kubernetes objects that already exist in the cluster, as context"`
	IncludeAll     bool             `json:"includeAll,omitempty" jsonschema:"also list the links that are ok or skipped"`
	Checks         []string         `json:"checks,omitempty" jsonschema:"only run these checks. An entry may use * as a wildcard, for example ingress-*. list_checks shows the names. Leave out to run every check"`
	SkipChecks     []string         `json:"skipChecks,omitempty" jsonschema:"leave these checks out. An entry may use * as a wildcard"`
}

type describeLinksInput struct {
	Manifests      string           `json:"manifests,omitempty" jsonschema:"inline Kubernetes YAML or JSON; several documents may be separated by ---"`
	Files          []string         `json:"files,omitempty" jsonschema:"manifest file paths inside the workspace, relative to the workspace root"`
	Directory      string           `json:"directory,omitempty" jsonschema:"a directory inside the workspace; every manifest under it is read"`
	Namespace      string           `json:"namespace,omitempty" jsonschema:"the namespace the bundle is deployed to; used for resources that set none"`
	BundleComplete bool             `json:"bundleComplete,omitempty" jsonschema:"set to true when the bundle is everything that will exist"`
	ClusterObjects []map[string]any `json:"clusterObjects,omitempty" jsonschema:"Kubernetes objects that already exist in the cluster, as context"`
	Checks         []string         `json:"checks,omitempty" jsonschema:"only run these checks. An entry may use * as a wildcard, for example ingress-*. list_checks shows the names. Leave out to run every check"`
	SkipChecks     []string         `json:"skipChecks,omitempty" jsonschema:"leave these checks out. An entry may use * as a wildcard"`
}

// DirectoryReport is a report whose findings are grouped by file and
// resource.
type DirectoryReport struct {
	CheckID        string              `json:"checkId"`
	Status         links.Verdict       `json:"status"`
	Complete       bool                `json:"complete"`
	Consistent     bool                `json:"consistent"`
	BundleComplete bool                `json:"bundleComplete"`
	ChecksRun      []string            `json:"checksRun"`
	ChecksSkipped  []string            `json:"checksSkipped,omitempty"`
	Directory      string              `json:"directory"`
	Summary        links.Summary       `json:"summary"`
	Files          []links.FileGroup   `json:"files"`
	SkippedFiles   []links.SkippedFile `json:"skippedFiles,omitempty"`
	Notes          []string            `json:"notes,omitempty"`
}

// Graph is what describe_links returns: the resources and every link
// between them.
type Graph struct {
	CheckID        string              `json:"checkId"`
	Status         links.Verdict       `json:"status"`
	Complete       bool                `json:"complete"`
	Consistent     bool                `json:"consistent"`
	BundleComplete bool                `json:"bundleComplete"`
	ChecksRun      []string            `json:"checksRun"`
	ChecksSkipped  []string            `json:"checksSkipped,omitempty"`
	Summary        links.Summary       `json:"summary"`
	Resources      []links.Ref         `json:"resources"`
	Links          []links.Link        `json:"links"`
	SkippedFiles   []links.SkippedFile `json:"skippedFiles,omitempty"`
	Notes          []string            `json:"notes,omitempty"`
}

// ---- loading ----

// load reads the bundle from whichever of the three inputs were given.
func (s *Server) load(inline string, files []string, dir string) ([]manifests.Resource, []links.SkippedFile, error) {
	lim := s.app.ManifestLimits()
	var resources []manifests.Resource
	var skipped []links.SkippedFile
	if strings.TrimSpace(inline) != "" {
		rs, err := manifests.Parse([]byte(inline), "inline", lim)
		if err != nil {
			return nil, nil, err
		}
		resources = append(resources, rs...)
	}
	for _, f := range files {
		rs, err := s.app.Workspace.LoadFile(f, lim)
		if err != nil {
			return nil, nil, err
		}
		resources = append(resources, rs...)
	}
	if dir != "" {
		rs, sk, err := s.app.Workspace.LoadDir(dir, s.app.DirLimits())
		if err != nil {
			return nil, nil, err
		}
		if len(rs) == 0 {
			return nil, nil, fmt.Errorf("no Kubernetes manifests found under %s (%d file(s) skipped)", dir, len(sk))
		}
		resources, skipped = append(resources, rs...), sk
	}
	if lim.MaxResources > 0 && len(resources) > lim.MaxResources {
		return nil, nil, fmt.Errorf("more than %d resources in one request", lim.MaxResources)
	}
	return resources, skipped, nil
}

// ClusterObjects turns the caller's context objects into resources. Each
// must be a Kubernetes object with a kind and a name.
func ClusterObjects(objs []map[string]any) ([]manifests.Resource, error) {
	var out []manifests.Resource
	for i, obj := range objs {
		rs, err := manifests.FromObject(obj, fmt.Sprintf("clusterObjects[%d]", i))
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

func (s *Server) run(ctx context.Context, who string, req checks.Request, clusterObjs []map[string]any) (*checks.Result, error) {
	cluster, err := ClusterObjects(clusterObjs)
	if err != nil {
		return nil, err
	}
	req.ClusterObjects = cluster
	res, err := s.app.Check(ctx, req)
	if err != nil {
		return nil, err
	}
	s.remember(who, res)
	return res, nil
}

// remember keeps what explain_finding needs from a check: its links. The
// manifests themselves are not kept, so no Secret value outlives the call
// that sent it.
func (s *Server) remember(who string, res *checks.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.history[who]
	if h == nil {
		h = &callerHistory{results: map[string]*checks.Result{}}
		s.history[who] = h
	}
	id := res.Report.CheckID
	h.results[id] = &checks.Result{Report: &links.Report{CheckID: id}, Links: res.Links}
	h.order = append(h.order, id)
	h.links += len(res.Links)
	for len(h.order) > 1 && (len(h.order) > maxHistory || h.links > maxHistoryLinks) {
		h.links -= len(h.results[h.order[0]].Links)
		delete(h.results, h.order[0])
		h.order = h.order[1:]
	}
}

// ---- tools ----

func (s *Server) checkManifests(ctx context.Context, req *mcp.CallToolRequest, in checkManifestsInput) (*mcp.CallToolResult, *links.Report, error) {
	if strings.TrimSpace(in.Manifests) == "" && len(in.Files) == 0 {
		return nil, nil, errors.New("provide inline YAML in 'manifests' or workspace paths in 'files'")
	}
	resources, skipped, err := s.load(in.Manifests, in.Files, "")
	if err != nil {
		return nil, nil, err
	}
	res, err := s.run(ctx, caller(req), checks.Request{Resources: resources, Namespace: in.Namespace, BundleComplete: in.BundleComplete, IncludeAll: in.IncludeAll, SkippedFiles: skipped, Checks: in.Checks, SkipChecks: in.SkipChecks}, in.ClusterObjects)
	if err != nil {
		return nil, nil, err
	}
	return nil, res.Report, nil
}

func (s *Server) checkDirectory(ctx context.Context, req *mcp.CallToolRequest, in checkDirectoryInput) (*mcp.CallToolResult, *DirectoryReport, error) {
	if in.Directory == "" {
		return nil, nil, errors.New("directory is required")
	}
	resources, skipped, err := s.load("", nil, in.Directory)
	if err != nil {
		return nil, nil, err
	}
	res, err := s.run(ctx, caller(req), checks.Request{Resources: resources, Namespace: in.Namespace, BundleComplete: in.BundleComplete, IncludeAll: in.IncludeAll, SkippedFiles: skipped, Checks: in.Checks, SkipChecks: in.SkipChecks}, in.ClusterObjects)
	if err != nil {
		return nil, nil, err
	}
	return nil, Directory(res, in.Directory), nil
}

// Directory converts a result into the grouped directory form.
func Directory(res *checks.Result, dir string) *DirectoryReport {
	r := res.Report
	fileOf := map[string]string{}
	for _, mr := range res.Resources {
		fileOf[mr.Ref().Key()] = mr.File
	}
	return &DirectoryReport{
		CheckID: r.CheckID, Status: r.Status, Complete: r.Complete, Consistent: r.Consistent, BundleComplete: r.BundleComplete,
		ChecksRun: r.ChecksRun, ChecksSkipped: r.ChecksSkipped, Directory: dir, Summary: r.Summary,
		Files:        links.GroupBySource(r, func(ref links.Ref) string { return fileOf[ref.Key()] }),
		SkippedFiles: r.SkippedFiles, Notes: r.Notes,
	}
}

func (s *Server) describeLinks(ctx context.Context, req *mcp.CallToolRequest, in describeLinksInput) (*mcp.CallToolResult, *Graph, error) {
	if strings.TrimSpace(in.Manifests) == "" && len(in.Files) == 0 && in.Directory == "" {
		return nil, nil, errors.New("provide inline YAML in 'manifests', workspace paths in 'files', or a workspace 'directory'")
	}
	resources, skipped, err := s.load(in.Manifests, in.Files, in.Directory)
	if err != nil {
		return nil, nil, err
	}
	res, err := s.run(ctx, caller(req), checks.Request{Resources: resources, Namespace: in.Namespace, BundleComplete: in.BundleComplete, SkippedFiles: skipped, Checks: in.Checks, SkipChecks: in.SkipChecks}, in.ClusterObjects)
	if err != nil {
		return nil, nil, err
	}
	return nil, GraphOf(res), nil
}

// GraphOf converts a result into the describe_links form.
func GraphOf(res *checks.Result) *Graph {
	r := res.Report
	all := res.Links
	if all == nil {
		all = []links.Link{}
	}
	return &Graph{CheckID: r.CheckID, Status: r.Status, Complete: r.Complete, Consistent: r.Consistent, BundleComplete: r.BundleComplete,
		ChecksRun: r.ChecksRun, ChecksSkipped: r.ChecksSkipped, Summary: r.Summary, Resources: r.Resources, Links: all, SkippedFiles: r.SkippedFiles, Notes: r.Notes}
}

type listChecksInput struct{}

// CheckList is what list_checks returns.
type CheckList struct {
	Checks []checks.Check `json:"checks"`
	// SkippedByServer names checks the operator turned off. A caller
	// cannot turn them back on.
	SkippedByServer []string `json:"skippedByServer"`
}

func (s *Server) listChecks(ctx context.Context, _ *mcp.CallToolRequest, _ listChecksInput) (*mcp.CallToolResult, *CheckList, error) {
	return nil, &CheckList{Checks: checks.List(), SkippedByServer: s.app.SkippedChecks()}, nil
}

type explainInput struct {
	FindingID string `json:"findingId" jsonschema:"the id of a finding or link from an earlier check, for example f-1a2b3c4d-003"`
}

func (s *Server) explainFinding(ctx context.Context, req *mcp.CallToolRequest, in explainInput) (*mcp.CallToolResult, *checks.Explanation, error) {
	// Finding ids have the form f-<checkId>-<number>.
	parts := strings.Split(in.FindingID, "-")
	if len(parts) != 3 || parts[0] != "f" {
		return nil, nil, fmt.Errorf("%q is not a finding id; use the id field of a finding returned by check_manifests", in.FindingID)
	}
	s.mu.Lock()
	var res *checks.Result
	ok := false
	if h := s.history[caller(req)]; h != nil {
		res, ok = h.results[parts[1]]
	}
	s.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("check %s is no longer available (only the last %d checks of this session are kept, fewer when they are large); check again and use the new finding id", parts[1], maxHistory)
	}
	ex, err := res.Explain(in.FindingID)
	if err != nil {
		return nil, nil, err
	}
	return nil, ex, nil
}

// Run serves MCP over stdio until the client disconnects.
func Run(ctx context.Context, a *app.App) error {
	_, srv := New(a)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// HTTPOptions configures the hosted mode.
type HTTPOptions struct {
	// Addr is the address to listen on, for example 127.0.0.1:8089.
	Addr string
	// KeysFile holds the API keys: one "name sha256" pair per line.
	KeysFile string
	// PerMinute and Burst set each key's request budget.
	PerMinute float64
	Burst     int
	// MaxInput is the largest manifest input accepted, in bytes.
	MaxInput int64
}

// RunHTTP serves MCP over HTTP to holders of an API key until ctx ends.
func RunHTTP(ctx context.Context, a *app.App, opts HTTPOptions) error {
	if opts.KeysFile == "" {
		return errors.New("--http needs --keys: the hosted server only answers callers with an API key")
	}
	if opts.PerMinute <= 0 || opts.Burst < 1 {
		return errors.New("--rate must be above 0 and --burst at least 1")
	}
	keys, err := OpenKeyStore(opts.KeysFile)
	if err != nil {
		return fmt.Errorf("reading API keys: %w", err)
	}
	_, srv := New(a)
	return ServeHTTP(ctx.Done(), opts.Addr, NewHTTPHandler(srv, keys, NewLimiters(opts.PerMinute, opts.Burst), opts.MaxInput))
}
