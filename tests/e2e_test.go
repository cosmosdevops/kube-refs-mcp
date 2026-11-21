// Package tests holds end-to-end tests. They build the real binary and talk
// to it the way an MCP client or a CI pipeline would. No cluster is used.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	binary   string
	repoRoot string
)

func TestMain(m *testing.M) {
	wd, _ := os.Getwd()
	repoRoot = filepath.Dir(wd)
	dir, err := os.MkdirTemp("", "kube-refs-mcp-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "kube-refs-mcp")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kube-refs-mcp")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func example(parts ...string) string {
	return filepath.Join(append([]string{repoRoot, "examples"}, parts...)...)
}

// connect starts the server over stdio, exactly as an MCP client does.
func connect(t *testing.T, args ...string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.Command(binary, append([]string{"serve"}, args...)...)
	// No kubeconfig and no cluster environment: the server must work offline.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KUBECONFIG=/nonexistent"}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type link struct {
	ID     string `json:"id"`
	Check  string `json:"check"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Field  string `json:"field"`
	From   struct {
		Kind, Name, Namespace string
	} `json:"from"`
	To struct {
		Kind, Name, Namespace, Key, Port, Selector string
	} `json:"to"`
	Message      string `json:"message"`
	ClosestMatch string `json:"closestMatch"`
	Fix          string `json:"fix"`
}

type summary struct {
	Resources, Links, OK, Broken, Unresolved, Skipped, Errors int
}

type report struct {
	CheckID        string   `json:"checkId"`
	Status         string   `json:"status"`
	Complete       bool     `json:"complete"`
	Consistent     bool     `json:"consistent"`
	BundleComplete bool     `json:"bundleComplete"`
	ChecksRun      []string `json:"checksRun"`
	ChecksSkipped  []string `json:"checksSkipped"`
	Notes          []string `json:"notes"`
	Summary        summary  `json:"summary"`
	Findings       []link   `json:"findings"`
	Links          []link   `json:"links"`
	Files          []struct {
		Path      string `json:"path"`
		Resources []struct {
			Status   string `json:"status"`
			Findings []link `json:"findings"`
		} `json:"resources"`
	} `json:"files"`
	SkippedFiles []struct{ Path, Reason string } `json:"skippedFiles"`
}

// byCheck returns the first finding of a check.
func byCheck(t *testing.T, fs []link, check string) link {
	t.Helper()
	for _, f := range fs {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no %s finding in %+v", check, fs)
	return link{}
}

// call invokes a tool and decodes its structured result into out.
func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any, out any) (isError bool, text string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return true, text
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) == "null" {
			raw = []byte(text)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: cannot decode result: %v\n%s", tool, err, raw)
		}
	}
	return false, text
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The loop an agent runs: check a draft, read the findings, ask about one,
// fix the draft, check again.
func TestAgentWorkflow(t *testing.T) {
	s := connect(t)

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "check_directory,check_manifests,describe_links,explain_finding,list_checks" {
		t.Fatalf("tools = %s", got)
	}

	draft := read(t, example("manifests", "broken", "checkout.yaml"))
	var rep report
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": draft}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Status != "fail" || rep.Consistent || rep.Complete {
		t.Errorf("verdict = %s complete=%t consistent=%t", rep.Status, rep.Complete, rep.Consistent)
	}
	if rep.Summary.Broken != 2 || rep.Summary.Unresolved != 2 || len(rep.Findings) != 4 {
		t.Errorf("summary = %+v, %d findings", rep.Summary, len(rep.Findings))
	}

	sel := byCheck(t, rep.Findings, "service-selector")
	if sel.Status != "unresolved" || sel.From.Kind != "Service" || sel.To.Selector != "app=checkuot" || !strings.Contains(sel.ClosestMatch, "Deployment/shop/checkout") {
		t.Errorf("selector finding = %+v", sel)
	}
	key := byCheck(t, rep.Findings, "configmap")
	if key.Status != "broken" || key.To.Key != "payment_url" || !strings.Contains(key.ClosestMatch, "payment-url") || !strings.HasSuffix(key.Field, "configMapKeyRef.key") {
		t.Errorf("key finding = %+v", key)
	}
	port := byCheck(t, rep.Findings, "ingress-backend")
	if port.Status != "broken" || port.To.Port != "8080" || port.Fix == "" {
		t.Errorf("port finding = %+v", port)
	}
	hpa := byCheck(t, rep.Findings, "hpa-target")
	if hpa.Status != "unresolved" || hpa.Reason != "not_in_bundle" || !strings.Contains(hpa.ClosestMatch, "Deployment checkout") {
		t.Errorf("hpa finding = %+v", hpa)
	}

	var ex struct {
		Summary    string   `json:"summary"`
		Meaning    string   `json:"meaning"`
		Expected   string   `json:"expected"`
		Found      string   `json:"found"`
		Candidates []string `json:"candidates"`
	}
	if isErr, text := call(t, s, "explain_finding", map[string]any{"findingId": key.ID}, &ex); isErr {
		t.Fatal(text)
	}
	if ex.Expected == "" || !strings.Contains(ex.Found, "payment-url") || !strings.Contains(ex.Meaning, "Certain") {
		t.Errorf("explanation = %+v", ex)
	}
	if isErr, text := call(t, s, "explain_finding", map[string]any{"findingId": "f-00000000-001"}, nil); !isErr || !strings.Contains(text, "no longer available") {
		t.Errorf("unknown finding: %v %s", isErr, text)
	}
	if isErr, _ := call(t, s, "explain_finding", map[string]any{"findingId": "nonsense"}, nil); !isErr {
		t.Error("a malformed id was accepted")
	}

	// Saying the bundle is complete turns the two open questions into
	// failures.
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": draft, "bundleComplete": true}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Summary.Broken != 4 || rep.Summary.Unresolved != 0 || !rep.Complete || !rep.BundleComplete {
		t.Errorf("complete bundle: %+v", rep)
	}

	// The fixed bundle is consistent.
	fixed := read(t, example("manifests", "consistent", "checkout.yaml"))
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": fixed, "bundleComplete": true}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Status != "pass" || !rep.Consistent || len(rep.Findings) != 0 {
		t.Errorf("fixed bundle: %+v", rep)
	}

	// describe_links shows the wiring, including what is fine.
	var graph report
	if isErr, text := call(t, s, "describe_links", map[string]any{"manifests": fixed}, &graph); isErr {
		t.Fatal(text)
	}
	if len(graph.Links) != 9 || graph.Summary.OK != 9 {
		t.Errorf("graph = %d links, %+v", len(graph.Links), graph.Summary)
	}
	svc := byCheck(t, graph.Links, "service-selector")
	if svc.Status != "ok" || svc.From.Kind != "Service" || svc.To.Kind != "Deployment" || svc.To.Name != "checkout" {
		t.Errorf("service link = %+v", svc)
	}
}

// An object that is not in the bundle is never a pass. Supplying it as
// context settles the question.
func TestUnresolvedAndClusterObjects(t *testing.T) {
	s := connect(t)
	worker := read(t, example("manifests", "unresolved", "worker.yaml"))

	var rep report
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": worker}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Status != "unknown" || rep.Complete || rep.Consistent || rep.Summary.Unresolved != 2 || rep.Summary.Broken != 0 {
		t.Errorf("report = %+v", rep)
	}

	cluster := []map[string]any{
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "registry-credentials", "namespace": "shop"}},
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "worker-data", "namespace": "shop"}},
	}
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": worker, "clusterObjects": cluster, "bundleComplete": true}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Status != "pass" || !rep.Consistent {
		t.Errorf("with cluster objects: %+v", rep)
	}

	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": worker, "clusterObjects": []map[string]any{{"kind": "Secret"}}}, nil); !isErr {
		t.Errorf("a cluster object with no name was accepted: %s", text)
	}
}

type checkList struct {
	Checks []struct {
		Name, Description, Decides string
		Kinds                      []string
	} `json:"checks"`
	SkippedByServer []string `json:"skippedByServer"`
}

// A caller can list the checks and turn some off for one call. What the
// operator turned off stays off.
func TestTurningChecksOff(t *testing.T) {
	s := connect(t)
	draft := read(t, example("manifests", "broken", "checkout.yaml"))

	var list checkList
	if isErr, text := call(t, s, "list_checks", nil, &list); isErr {
		t.Fatal(text)
	}
	if len(list.Checks) < 16 || len(list.SkippedByServer) != 0 || list.Checks[0].Decides == "" {
		t.Errorf("list_checks = %+v", list)
	}

	var rep report
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": draft, "skipChecks": []string{"ingress-*", "configmap"}}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Summary.Broken != 0 || rep.Summary.Unresolved != 2 || strings.Join(rep.ChecksSkipped, ",") != "configmap,ingress-backend,ingress-tls" {
		t.Errorf("with checks skipped: %+v", rep)
	}
	if len(rep.Notes) == 0 || !strings.Contains(rep.Notes[len(rep.Notes)-1], "turned off") {
		t.Errorf("notes = %v", rep.Notes)
	}
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": draft, "checks": []string{"hpa-target"}}, &rep); isErr || rep.Summary.Links != 1 || len(rep.ChecksRun) != 1 {
		t.Errorf("one check: %v %s %+v", isErr, text, rep)
	}
	if isErr, text := call(t, s, "describe_links", map[string]any{"manifests": draft, "skipChecks": []string{"hpa-target"}}, &rep); isErr || len(rep.Links) != 5 || len(rep.ChecksSkipped) != 1 {
		t.Errorf("describe_links: %v %s %+v", isErr, text, rep)
	}
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": draft, "skipChecks": []string{"hpa-taget"}}, nil); !isErr || !strings.Contains(text, "no check is named") {
		t.Errorf("mistyped check: %v %s", isErr, text)
	}

	// Turned off by the operator: off for every call, and a caller cannot
	// turn it back on.
	off := connect(t, "--skip-check", "hpa-target")
	if isErr, text := call(t, off, "list_checks", nil, &list); isErr || strings.Join(list.SkippedByServer, ",") != "hpa-target" {
		t.Errorf("list_checks: %v %s %+v", isErr, text, list.SkippedByServer)
	}
	if isErr, text := call(t, off, "check_manifests", map[string]any{"manifests": draft}, &rep); isErr || rep.Summary.Unresolved != 1 || strings.Join(rep.ChecksSkipped, ",") != "hpa-target" {
		t.Errorf("operator skip: %v %s %+v", isErr, text, rep)
	}
	if isErr, _ := call(t, off, "check_manifests", map[string]any{"manifests": draft, "checks": []string{"hpa-target"}}, nil); !isErr {
		t.Error("a caller turned a check back on")
	}
}

// The table of checks in the docs is the same list the program has.
func TestDocsListEveryCheck(t *testing.T) {
	code, out := runCLI(t, "", "checks", "-o", "json")
	var list []struct{ Name string }
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	var inCode, inDocs []string
	for _, c := range list {
		inCode = append(inCode, c.Name)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(read(t, filepath.Join(repoRoot, "docs", "checks.md")), "\n") {
		// The first cell of a table row, when it is a name in backticks.
		if rest, ok := strings.CutPrefix(line, "| `"); ok {
			if name, _, ok := strings.Cut(rest, "` |"); ok && !seen[name] {
				seen[name] = true
				inDocs = append(inDocs, name)
			}
		}
	}
	sort.Strings(inCode)
	sort.Strings(inDocs)
	if strings.Join(inCode, ",") != strings.Join(inDocs, ",") {
		t.Errorf("docs/checks.md and the program disagree:\n  program: %v\n  docs:    %v", inCode, inDocs)
	}
}

func TestFilesDirectoryAndWorkspaceLimits(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "app"), 0o755))
	must(os.WriteFile(filepath.Join(root, "app", "deploy.yaml"), []byte(read(t, example("manifests", "unresolved", "worker.yaml"))), 0o644))
	must(os.WriteFile(filepath.Join(root, "app", "claim.yaml"), []byte("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: worker-data, namespace: shop}\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "app", "values.yaml"), []byte("replicaCount: 2\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "app", "template.yaml"), []byte("kind: {{ .Values.kind }}\n  bad: [\n"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: registry-credentials, namespace: shop}\n"), 0o644))
	must(os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(root, "link.yaml")))

	s := connect(t, "--workspace-root", root)

	// The directory is one bundle: the claim in one file resolves the
	// reference in another.
	var rep report
	if isErr, text := call(t, s, "check_directory", map[string]any{"directory": "app"}, &rep); isErr {
		t.Fatal(text)
	}
	if rep.Summary.Resources != 2 || rep.Summary.Unresolved != 1 || rep.Status != "unknown" {
		t.Errorf("directory report = %+v", rep)
	}
	if len(rep.Files) != 2 || rep.Files[0].Path != "app/claim.yaml" || rep.Files[1].Path != "app/deploy.yaml" {
		t.Errorf("files = %+v", rep.Files)
	}
	if got := rep.Files[1].Resources[0]; got.Status != "unresolved" || len(got.Findings) != 1 || got.Findings[0].To.Name != "registry-credentials" {
		t.Errorf("deploy.yaml = %+v", got)
	}
	if len(rep.SkippedFiles) != 2 {
		t.Errorf("skipped = %+v", rep.SkippedFiles)
	}

	if isErr, text := call(t, s, "check_manifests", map[string]any{"files": []string{"app/deploy.yaml", "app/claim.yaml"}}, &rep); isErr || rep.Summary.Unresolved != 1 {
		t.Errorf("files: %v %s %+v", isErr, text, rep.Summary)
	}
	if isErr, text := call(t, s, "describe_links", map[string]any{"directory": "app"}, &rep); isErr || len(rep.Links) != 3 {
		t.Errorf("describe_links on a directory: %v %s %d links", isErr, text, len(rep.Links))
	}

	// Nothing outside the workspace is read: not by path, not by "..", and
	// not through a symlink.
	for _, path := range []string{filepath.Join(outside, "secret.yaml"), "../" + filepath.Base(outside) + "/secret.yaml", "link.yaml", "/etc/passwd"} {
		isErr, text := call(t, s, "check_manifests", map[string]any{"files": []string{path}}, nil)
		if !isErr || !strings.Contains(text, "outside the configured workspace roots") {
			t.Errorf("%s: isError=%t %s", path, isErr, text)
		}
	}
	if isErr, _ := call(t, s, "check_directory", map[string]any{"directory": outside}, nil); !isErr {
		t.Error("a directory outside the workspace was read")
	}

	// With no workspace root there is no file access at all.
	bare := connect(t)
	if isErr, text := call(t, bare, "check_manifests", map[string]any{"files": []string{"app/deploy.yaml"}}, nil); !isErr || !strings.Contains(text, "file access is disabled") {
		t.Errorf("no workspace: isError=%t %s", isErr, text)
	}
}

func TestInputsAreBounded(t *testing.T) {
	s := connect(t, "--max-manifest-bytes", "200", "--max-resources", "2")
	big := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: big}\ndata: {k: " + strings.Repeat("x", 300) + "}\n"
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": big}, nil); !isErr || !strings.Contains(text, "byte limit") {
		t.Errorf("oversized input: %v %s", isErr, text)
	}
	many := ""
	for i := 0; i < 3; i++ {
		many += fmt.Sprintf("---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c%d}\n", i)
	}
	if isErr, text := call(t, s, "check_manifests", map[string]any{"manifests": many}, nil); !isErr || !strings.Contains(text, "more than 2 resources") {
		t.Errorf("too many resources: %v %s", isErr, text)
	}
	// Input that is not YAML is an error, never an empty pass.
	if isErr, _ := call(t, s, "check_manifests", map[string]any{"manifests": "kind: [unclosed"}, nil); !isErr {
		t.Error("broken YAML was accepted")
	}
	if isErr, _ := call(t, s, "check_manifests", map[string]any{}, nil); !isErr {
		t.Error("an empty request was accepted")
	}
}

func TestSecretsAreNotEchoed(t *testing.T) {
	s := connect(t)
	bundle := `
apiVersion: v1
kind: Secret
metadata: {name: db}
data: {password: c3VwZXItc2VjcmV0LXB3}
stringData: {token: tok-1234567890}
---
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: c
      image: example.com/app:1
      env:
        - {name: A, valueFrom: {secretKeyRef: {name: db, key: pasword}}}
`
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "check_manifests", Arguments: map[string]any{"manifests": bundle, "includeAll": true}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var rep report
	call(t, s, "check_manifests", map[string]any{"manifests": bundle}, &rep)
	exp, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "explain_finding", Arguments: map[string]any{"findingId": rep.Findings[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	more, _ := json.Marshal(exp)
	for _, leak := range []string{"c3VwZXItc2VjcmV0LXB3", "super-secret-pw", "tok-1234567890"} {
		if bytes.Contains(raw, []byte(leak)) || bytes.Contains(more, []byte(leak)) {
			t.Errorf("the output contains the secret value %q", leak)
		}
	}
}

func runCLI(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

func TestCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"consistent", []string{"check", "--dir", "examples/manifests/consistent"}, 0, "Verdict:   PASS"},
		{"broken", []string{"check", "--dir", "examples/manifests/broken"}, 1, "closest match: key payment-url"},
		{"unresolved", []string{"check", "-f", "examples/manifests/unresolved/worker.yaml"}, 2, "Verdict:   UNKNOWN"},
		{"complete", []string{"check", "-f", "examples/manifests/unresolved/worker.yaml", "--complete"}, 1, "The bundle was said to be complete"},
		{"cluster objects", []string{"check", "-f", "examples/manifests/unresolved/worker.yaml", "--cluster-objects", "examples/cluster-objects/worker.yaml", "--complete"}, 0, "Verdict:   PASS"},
		{"links", []string{"links", "--dir", "examples/manifests/broken"}, 0, "BROKEN      ingress-backend"},
		{"skip checks", []string{"check", "--dir", "examples/manifests/broken", "--skip-check", "configmap", "--skip-check", "ingress-*"}, 2, "3 check(s) were turned off"},
		{"one check", []string{"check", "--dir", "examples/manifests/broken", "--check", "workload-selector"}, 0, "Verdict:   PASS"},
		{"mistyped check", []string{"check", "--dir", "examples/manifests/broken", "--skip-check", "confgimap"}, 3, "no check is named"},
		{"list checks", []string{"checks"}, 0, "service-selector"},
		{"serve with a mistyped check", []string{"serve", "--skip-check", "nope"}, 3, "no check is named"},
		{"no input", []string{"check"}, 3, "give at least one --file or a --dir"},
		{"missing file", []string{"check", "-f", "nope.yaml"}, 3, "no such file"},
		{"bad format", []string{"check", "--dir", "examples/manifests/consistent", "-o", "xml"}, 3, "unknown output format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runCLI(t, "", tc.args...)
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Errorf("exit %d (want %d), output:\n%s", code, tc.code, out)
			}
		})
	}

	// Manifests from stdin, JSON out.
	code, out := runCLI(t, read(t, example("manifests", "broken", "checkout.yaml")), "check", "-f", "-", "-o", "json", "--explain")
	var payload struct {
		Report       report `json:"report"`
		Explanations []struct {
			Summary string `json:"summary"`
		} `json:"explanations"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if code != 1 || payload.Report.Status != "fail" || len(payload.Explanations) != 4 {
		t.Errorf("exit %d, report %+v", code, payload.Report.Summary)
	}

	if code, out := runCLI(t, "", "version"); code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out := runCLI(t, "", "serve", "--http", "127.0.0.1:0"); code != 3 || !strings.Contains(out, "--keys") {
		t.Errorf("serve --http without keys: %d %s", code, out)
	}
}
