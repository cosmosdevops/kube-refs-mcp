package manifests

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	// A directory walk must not follow the symlinks either.
	rs, _, err := ws.LoadDir(".", DirLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Kind() != "Pod" {
		t.Errorf("walk followed a symlink: %v", rs)
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
		t.Errorf("want 2 skipped files, got %+v", skipped)
	}
}
