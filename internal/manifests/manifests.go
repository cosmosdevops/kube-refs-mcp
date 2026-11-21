// Package manifests loads Kubernetes manifests just far enough to identify
// resources and hand them to the checks. It does not validate schemas.
package manifests

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

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

// Parse reads one or more YAML or JSON documents. Lists are flattened. A
// document that is not a Kubernetes resource yields a *NotManifestError.
func Parse(data []byte, file string, lim Limits) ([]Resource, error) {
	if lim.MaxBytes > 0 && int64(len(data)) > lim.MaxBytes {
		return nil, fmt.Errorf("%s: input is %d bytes, larger than the %d byte limit", file, len(data), lim.MaxBytes)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []Resource
	for idx := 0; ; idx++ {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d is not valid YAML or JSON: %w", file, idx, err)
		}
		if doc == nil {
			continue
		}
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, &NotManifestError{Reason: fmt.Sprintf("%s: document %d is not a Kubernetes object", file, idx)}
		}
		source := fmt.Sprintf("%s#%d", file, idx)
		items, err := flatten(obj, source, file)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if lim.MaxResources > 0 && len(out) > lim.MaxResources {
			return nil, fmt.Errorf("%s: more than %d resources in one request", file, lim.MaxResources)
		}
	}
	return out, nil
}

// FromObject turns an object that is already decoded into resources. A
// List is flattened, as in Parse.
func FromObject(obj map[string]any, source string) ([]Resource, error) {
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

// IsClusterScoped reports whether kind is a well-known cluster-scoped kind.
func IsClusterScoped(kind string) bool { return clusterScoped[kind] }

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
