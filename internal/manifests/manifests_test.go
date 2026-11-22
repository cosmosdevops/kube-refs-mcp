package manifests

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const multi = `
apiVersion: v1
kind: ConfigMap
metadata: {name: a, namespace: x}
---
# empty document below
---
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Service
    metadata: {name: b}
  - apiVersion: apps/v1
    kind: Deployment
    metadata: {generateName: c-}
`

func TestParseMultiDocumentAndLists(t *testing.T) {
	rs, err := Parse([]byte(multi), "inline", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("want 3 resources, got %d", len(rs))
	}
	if got := rs[0].Ref().String(); got != "ConfigMap/x/a" {
		t.Errorf("ref = %s", got)
	}
	if rs[2].Name() != "c-*" {
		t.Errorf("generateName not handled: %q", rs[2].Name())
	}
	if rs[1].Source != "inline#2.items[0]" {
		t.Errorf("source = %q", rs[1].Source)
	}
}

func TestParseJSON(t *testing.T) {
	rs, err := Parse([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"}}`), "x.json", Limits{})
	if err != nil || len(rs) != 1 || rs[0].Kind() != "Pod" {
		t.Fatalf("rs=%v err=%v", rs, err)
	}
}

func TestParseRejectsNonManifests(t *testing.T) {
	for name, in := range map[string]string{
		"values file":   "replicaCount: 2\nimage: nginx\n",
		"no name":       "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [a.yaml]\n",
		"scalar":        "just a string\n",
		"list of items": "- a\n- b\n",
	} {
		_, err := Parse([]byte(in), "f.yaml", Limits{})
		var nm *NotManifestError
		if !errors.As(err, &nm) {
			t.Errorf("%s: want NotManifestError, got %v", name, err)
		}
	}
	if _, err := Parse([]byte("a: [unclosed"), "f.yaml", Limits{}); err == nil {
		t.Error("invalid YAML must fail")
	}
}

func TestParseLimits(t *testing.T) {
	if _, err := Parse([]byte(multi), "inline", Limits{MaxBytes: 10}); err == nil {
		t.Error("byte limit not enforced")
	}
	if _, err := Parse([]byte(multi), "inline", Limits{MaxResources: 2}); err == nil {
		t.Error("resource limit not enforced")
	}
}

func TestApplyDefaultNamespaceDoesNotMutate(t *testing.T) {
	rs, err := Parse([]byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\n---\napiVersion: v1\nkind: Namespace\nmetadata: {name: ns1}\n"), "inline", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	out, changed := ApplyDefaultNamespace(rs, "team-a")
	if changed != 1 || out[0].Namespace() != "team-a" || out[1].Namespace() != "" {
		t.Fatalf("changed=%d ns=%q/%q", changed, out[0].Namespace(), out[1].Namespace())
	}
	if rs[0].Namespace() != "" {
		t.Error("input resource was modified")
	}
}

func TestWorkspaceBlocksEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.yaml")
	os.WriteFile(secret, []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: s}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "ok.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\n"), 0o600)
	os.Symlink(secret, filepath.Join(root, "link.yaml"))
	os.Symlink(outside, filepath.Join(root, "linkdir"))

	ws, err := NewWorkspace([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.LoadFile("ok.yaml", Limits{}); err != nil {
		t.Fatalf("file inside workspace: %v", err)
	}
	for _, p := range []string{"../" + filepath.Base(outside) + "/secret.yaml", secret, "link.yaml", "linkdir/secret.yaml", "/etc/passwd", "a\x00b"} {
		if _, err := ws.LoadFile(p, Limits{}); err == nil {
			t.Errorf("path %q escaped the workspace", p)
		}
	}
	// A path that does not exist gets the same answer whether or not it
	// passes through a link that leaves the workspace. Otherwise the answer
	// would say which files exist outside.
	_, missing := ws.LoadFile("linkdir/nope.yaml", Limits{})
	_, present := ws.LoadFile("linkdir/secret.yaml", Limits{})
	if missing == nil || present == nil || strings.Replace(missing.Error(), "nope", "secret", 1) != present.Error() {
		t.Errorf("answers differ: %v / %v", missing, present)
	}
	if _, err := ws.LoadFile("nope.yaml", Limits{}); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("a missing file inside the workspace: %v", err)
	}
	// A directory walk must not follow the symlinks either. It says that it
	// did not read the linked file.
	rs, skipped, err := ws.LoadDir(".", DirLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Kind() != "Pod" {
		t.Errorf("walk followed a symlink: %v", rs)
	}
	if len(skipped) != 1 || skipped[0].Path != "link.yaml" || !skipped[0].NotRead {
		t.Errorf("skipped = %+v", skipped)
	}
	empty, _ := NewWorkspace(nil)
	if _, err := empty.Resolve("ok.yaml"); err == nil {
		t.Error("file access must be off without workspace roots")
	}
}

func TestLoadDirSkipsNonManifests(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub", ".git"), 0o700)
	os.WriteFile(filepath.Join(root, "sub", "a.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "values.yaml"), []byte("replicas: 2\n"), 0o600)
	os.WriteFile(filepath.Join(root, "tmpl.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: {{ .Release.Name }}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "sub", ".git", "x.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: hidden}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi"), 0o600)
	ws, _ := NewWorkspace([]string{root})
	rs, skipped, err := ws.LoadDir(".", DirLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].File != filepath.Join("sub", "a.yaml") {
		t.Errorf("resources = %+v", rs)
	}
	if len(skipped) != 2 {
		t.Fatalf("want 2 skipped files, got %+v", skipped)
	}
	// The template is not valid YAML, so what it holds is unknown. The
	// values file was read and holds no resource.
	if skipped[0].Path != "tmpl.yaml" || !skipped[0].NotRead || skipped[1].Path != "values.yaml" || skipped[1].NotRead {
		t.Errorf("skipped = %+v", skipped)
	}
}

// One document that is not a resource must not hide the others in its file.
func TestLoadDirKeepsTheRestOfAFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "mixed.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\n---\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [a.yaml]\n---\napiVersion: v1\nkind: Service\nmetadata: {name: s}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "big.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: big}\ndata: {a: "+strings.Repeat("x", 500)+"}\n"), 0o600)
	ws, _ := NewWorkspace([]string{root})
	rs, skipped, err := ws.LoadDir(".", DirLimits{Limits: Limits{MaxBytes: 300}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Kind() != "Pod" || rs[1].Kind() != "Service" {
		t.Errorf("resources = %+v", rs)
	}
	if len(skipped) != 2 || !skipped[0].NotRead || skipped[1].NotRead || skipped[1].Reason != "document 1: Kustomization has no metadata.name, so it is not a deployable Kubernetes resource" || strings.Contains(skipped[0].Reason, "big.yaml") {
		t.Errorf("skipped = %+v", skipped)
	}
	// Named directly, the same file is still refused as a whole.
	if _, err := ws.LoadFile("mixed.yaml", Limits{}); err == nil {
		t.Error("a file with a document that is not a resource was accepted")
	}
}

// A pipe never ends. Reading one must fail at once, not hang.
func TestPipesAreNotRead(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.yaml"), 0o600); err != nil {
		t.Skip("cannot make a pipe here:", err)
	}
	ws, _ := NewWorkspace([]string{root})
	done := make(chan error, 1)
	go func() {
		_, err := ws.LoadFile("pipe.yaml", Limits{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a pipe did not return")
	}
}

// An alias repeats an anchored value. A small file must not be allowed to
// stand for a document larger than the limit.
func TestAliasesCannotExpandPastTheLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  a: &a " + strings.Repeat("x", 5000) + "\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "  b%d: *a\n", i)
	}
	if _, err := Parse([]byte(b.String()), "f.yaml", Limits{MaxBytes: 100000}); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Errorf("err = %v", err)
	}
	// Ordinary use of an anchor is fine, and so is an ampersand in JSON.
	small := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n  labels: &l {app: web}\ndata:\n  note: \"a & b * c\"\nextra: *l\n"
	if rs, err := Parse([]byte(small), "f.yaml", Limits{MaxBytes: 100000}); err != nil || len(rs) != 1 {
		t.Errorf("small anchor: %v %v", rs, err)
	}
	if _, err := Parse([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"a":"x & y * z"}}`), "f.json", Limits{MaxBytes: 100000}); err != nil {
		t.Errorf("JSON: %v", err)
	}
}

func TestOverlongNamesAreRefused(t *testing.T) {
	long := strings.Repeat("a", MaxNameLength+1)
	for name, doc := range map[string]string{
		"name":      "apiVersion: v1\nkind: Pod\nmetadata: {name: " + long + "}\n",
		"namespace": "apiVersion: v1\nkind: Pod\nmetadata: {name: p, namespace: " + long + "}\n",
		"label key": "apiVersion: v1\nkind: Pod\nmetadata: {name: p, labels: {" + long + ": x}}\n",
		"data key":  "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {" + long + ": x}\n",
		"in a list": "apiVersion: v1\nkind: List\nitems: [{apiVersion: v1, kind: Pod, metadata: {name: " + long + "}}]\n",
	} {
		if _, err := Parse([]byte(doc), "f.yaml", Limits{}); err == nil || !strings.Contains(err.Error(), "characters long") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A long value is fine: a ConfigMap may hold a whole file.
	if _, err := Parse([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {a: "+long+"}\n"), "f.yaml", Limits{}); err != nil {
		t.Error(err)
	}
}

// A parse error must not quote the document, which may hold a secret.
func TestParseErrorsDoNotQuoteTheDocument(t *testing.T) {
	_, err := Parse([]byte("apiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData:\n  ? [a, hunter2-SECRET]\n  : b\n"), "f.yaml", Limits{})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v", err)
	}
}
