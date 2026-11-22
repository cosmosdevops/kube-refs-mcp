// Package manifests loads Kubernetes manifests just far enough to identify
// resources and hand them to the checks. It does not validate schemas.
package manifests

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	yaml "go.yaml.in/yaml/v3"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// Resource is one Kubernetes object taken from a manifest.
type Resource struct {
	Object map[string]any
	// Source is the file path or "inline", plus the document index.
	Source string
	// File is the file path alone ("inline" for inline input).
	File string
}

// APIVersion returns the object's apiVersion.
func (r Resource) APIVersion() string { return str(r.Object["apiVersion"]) }

// Kind returns the object's kind.
func (r Resource) Kind() string { return str(r.Object["kind"]) }

// Name returns metadata.name, or metadata.generateName with a "*" suffix.
func (r Resource) Name() string {
	md, _ := r.Object["metadata"].(map[string]any)
	if n := str(md["name"]); n != "" {
		return n
	}
	if g := str(md["generateName"]); g != "" {
		return g + "*"
	}
	return ""
}

// Namespace returns metadata.namespace.
func (r Resource) Namespace() string {
	md, _ := r.Object["metadata"].(map[string]any)
	return str(md["namespace"])
}

// Ref returns the resource's identity.
func (r Resource) Ref() links.Ref {
	return links.Ref{APIVersion: r.APIVersion(), Kind: r.Kind(), Namespace: r.Namespace(), Name: r.Name(), Source: r.Source}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Limits bounds manifest input.
type Limits struct {
	MaxBytes     int64
	MaxResources int
}

// NotManifestError means the input parsed but is not a Kubernetes resource.
type NotManifestError struct{ Reason string }

func (e *NotManifestError) Error() string { return e.Reason }

// MaxNameLength bounds a resource's name, namespace, kind and apiVersion,
// and every field name. Kubernetes allows at most 253 characters for a
// name and 317 for a label key, so anything longer is not a manifest it
// would accept. The bound keeps one long name from being copied into
// every message.
const MaxNameLength = 512

// Parse reads one or more YAML or JSON documents. Lists are flattened. A
// document that is not a Kubernetes resource yields a *NotManifestError.
func Parse(data []byte, file string, lim Limits) ([]Resource, error) {
	out, notResources, err := ParseLenient(data, file, lim)
	if err != nil {
		return nil, err
	}
	if len(notResources) > 0 {
		return nil, &NotManifestError{Reason: notResources[0]}
	}
	return out, nil
}

// ParseLenient is Parse for a file found by walking a directory. A document
// that is not a Kubernetes resource is left out and described in
// notResources, and the other documents are still returned. Input that
// cannot be read at all is an error.
func ParseLenient(data []byte, file string, lim Limits) (out []Resource, notResources []string, err error) {
	if lim.MaxBytes > 0 && int64(len(data)) > lim.MaxBytes {
		return nil, nil, fmt.Errorf("%s: input is %d bytes, larger than the %d byte limit", file, len(data), lim.MaxBytes)
	}
	if err := checkAliases(data, file, lim.MaxBytes); err != nil {
		return nil, nil, err
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for idx := 0; ; idx++ {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: document %d is not valid YAML or JSON: %s", file, idx, parseErrorText(err))
		}
		if doc == nil {
			continue
		}
		source := fmt.Sprintf("%s#%d", file, idx)
		obj, ok := doc.(map[string]any)
		if !ok {
			notResources = append(notResources, fmt.Sprintf("%s: document %d is not a Kubernetes object", file, idx))
			continue
		}
		if err := checkFieldNames(obj, source); err != nil {
			return nil, nil, err
		}
		items, err := flatten(obj, source, file)
		var not *NotManifestError
		if errors.As(err, &not) {
			notResources = append(notResources, not.Reason)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, items...)
		if lim.MaxResources > 0 && len(out) > lim.MaxResources {
			return nil, nil, fmt.Errorf("%s: more than %d resources in one request", file, lim.MaxResources)
		}
	}
	return out, notResources, nil
}

// parseErrorText returns a parser's message without any of the document's
// content. Most messages only name a line. The one that quotes the
// document is replaced.
func parseErrorText(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "invalid map key"); i >= 0 {
		return msg[:i] + "a map key is not a plain string"
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// checkAliases refuses YAML whose aliases would expand past the size
// limit. An alias (*name) repeats an anchored value (&name), so a small
// file can stand for a very large document. The decoder's own guard counts
// nodes, not bytes, and lets one large value through many times.
func checkAliases(data []byte, file string, maxBytes int64) error {
	if maxBytes <= 0 || !bytes.ContainsRune(data, '&') || !bytes.ContainsRune(data, '*') {
		return nil
	}
	// The decoder reads input that starts with "{" as JSON, which has no
	// aliases.
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for idx := 0; ; idx++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: document %d is not valid YAML or JSON: %s", file, idx, parseErrorText(err))
		}
		sizes := map[*yaml.Node]int64{}
		if expandedSize(&doc, sizes, maxBytes, 0) > maxBytes {
			return fmt.Errorf("%s: document %d uses YAML aliases that expand to more than the %d byte limit", file, idx, maxBytes)
		}
	}
}

// expandedSize is the size of a node once every alias under it is written
// out in full. It stops counting once past the limit.
func expandedSize(n *yaml.Node, sizes map[*yaml.Node]int64, limit int64, depth int) int64 {
	if n == nil {
		return 0
	}
	if depth > 10000 {
		return limit + 1
	}
	if n.Kind == yaml.AliasNode {
		return expandedSize(n.Alias, sizes, limit, depth+1)
	}
	if size, ok := sizes[n]; ok {
		return size
	}
	size := int64(len(n.Value)) + 1
	for _, child := range n.Content {
		size += expandedSize(child, sizes, limit, depth+1)
		if size > limit {
			break
		}
	}
	sizes[n] = size
	return size
}

// checkFieldNames refuses an object with a field name longer than
// MaxNameLength, at any depth. Label keys and the keys of a ConfigMap or
// Secret are field names, and both are copied into messages.
func checkFieldNames(v any, source string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if len(k) > MaxNameLength {
				return fmt.Errorf("%s: a field name is %d characters long, more than the limit of %d", source, len(k), MaxNameLength)
			}
			if err := checkFieldNames(child, source); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range t {
			if err := checkFieldNames(child, source); err != nil {
				return err
			}
		}
	}
	return nil
}

// FromObject turns an object that is already decoded into resources. A
// List is flattened, as in Parse.
func FromObject(obj map[string]any, source string) ([]Resource, error) {
	if err := checkFieldNames(obj, source); err != nil {
		return nil, err
	}
	return flatten(obj, source, source)
}

func flatten(obj map[string]any, source, file string) ([]Resource, error) {
	kind := str(obj["kind"])
	if str(obj["apiVersion"]) == "" || kind == "" {
		return nil, &NotManifestError{Reason: fmt.Sprintf("%s: missing apiVersion or kind, so this is not a Kubernetes resource", source)}
	}
	if items, ok := obj["items"].([]any); ok && strings.HasSuffix(kind, "List") {
		var out []Resource
		for i, it := range items {
			child, ok := it.(map[string]any)
			if !ok {
				return nil, &NotManifestError{Reason: fmt.Sprintf("%s: list item %d is not an object", source, i)}
			}
			rs, err := flatten(child, fmt.Sprintf("%s.items[%d]", source, i), file)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		}
		return out, nil
	}
	r := Resource{Object: obj, Source: source, File: file}
	for field, value := range map[string]string{"metadata.name": r.Name(), "metadata.namespace": r.Namespace(), "kind": kind, "apiVersion": r.APIVersion()} {
		if len(value) > MaxNameLength {
			return nil, fmt.Errorf("%s: %s is %d characters long, more than the limit of %d", source, field, len(value), MaxNameLength)
		}
	}
	if r.Name() == "" {
		return nil, &NotManifestError{Reason: fmt.Sprintf("%s: %s has no metadata.name, so it is not a deployable Kubernetes resource", source, kind)}
	}
	return []Resource{r}, nil
}

// clusterScoped lists common kinds that never carry a namespace. This is only
// used to decide whether to apply the target namespace to a resource; it is
// not schema validation.
var clusterScoped = map[string]bool{
	"Namespace": true, "Node": true, "PersistentVolume": true, "ClusterRole": true,
	"ClusterRoleBinding": true, "CustomResourceDefinition": true, "StorageClass": true,
	"PriorityClass": true, "IngressClass": true, "RuntimeClass": true, "VolumeAttachment": true,
	"CSIDriver": true, "CSINode": true, "APIService": true, "CertificateSigningRequest": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
	"ClusterIssuer": true, "FlowSchema": true, "PriorityLevelConfiguration": true,
}

// extraClusterScoped holds kinds that rules files say have no namespace.
var extraClusterScoped = map[string]bool{}

// SetExtraClusterScoped replaces the kinds that are cluster-scoped besides
// the well-known ones. It is meant for start-up, when rules are installed.
func SetExtraClusterScoped(kinds []string) {
	extra := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		extra[k] = true
	}
	extraClusterScoped = extra
}

// IsClusterScoped reports whether kind is a well-known cluster-scoped kind,
// or one a rules file named as such.
func IsClusterScoped(kind string) bool { return clusterScoped[kind] || extraClusterScoped[kind] }

// ApplyDefaultNamespace returns copies of the resources where namespaced
// resources without a namespace are placed in ns. The input is not modified.
// It returns the number of resources that were changed.
func ApplyDefaultNamespace(in []Resource, ns string) ([]Resource, int) {
	if ns == "" {
		return in, 0
	}
	out := make([]Resource, len(in))
	changed := 0
	for i, r := range in {
		out[i] = r
		if r.Namespace() != "" || IsClusterScoped(r.Kind()) {
			continue
		}
		obj := make(map[string]any, len(r.Object))
		for k, v := range r.Object {
			obj[k] = v
		}
		md := map[string]any{}
		if old, ok := r.Object["metadata"].(map[string]any); ok {
			for k, v := range old {
				md[k] = v
			}
		}
		md["namespace"] = ns
		obj["metadata"] = md
		out[i].Object = obj
		changed++
	}
	return out, changed
}
