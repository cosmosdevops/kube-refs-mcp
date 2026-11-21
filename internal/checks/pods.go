package checks

import (
	"fmt"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// podTemplate is the pod a resource would create: the pod itself, or the
// template of a workload.
type podTemplate struct {
	owner  *object
	labels map[string]string
	spec   map[string]any
	// path is where the pod spec sits in the owner, for field paths.
	path string
}

// templatesOf returns the pod template of a resource, if it has one. The
// known workload kinds are read by name. Any other kind that has the usual
// spec.template shape is read the same way, which covers custom workloads
// such as an Argo Rollout.
func templatesOf(o *object) []*podTemplate {
	obj := o.res.Object
	var meta, spec map[string]any
	var path string
	switch o.gk {
	case kindPod:
		meta, spec, path = getMap(obj, "metadata"), getMap(obj, "spec"), "spec"
	case kindCronJob:
		tpl := getMap(obj, "spec", "jobTemplate", "spec", "template")
		meta, spec, path = getMap(tpl, "metadata"), getMap(tpl, "spec"), "spec.jobTemplate.spec.template.spec"
	default:
		tpl := getMap(obj, "spec", "template")
		meta, spec, path = getMap(tpl, "metadata"), getMap(tpl, "spec"), "spec.template.spec"
		if _, ok := spec["containers"].([]any); !ok {
			return nil
		}
	}
	if spec == nil {
		return nil
	}
	labels := stringMap(getMap(meta, "labels"))
	// A Job's pods always carry the Job's name. The other labels a
	// controller adds have values that are only known at run time.
	if o.gk == kindJob {
		for _, k := range []string{"job-name", "batch.kubernetes.io/job-name"} {
			if _, set := labels[k]; !set {
				labels[k] = o.name
			}
		}
	}
	return []*podTemplate{{owner: o, labels: labels, spec: spec, path: path}}
}

// runtimeLabels are labels the cluster adds to running pods, with values
// that a manifest does not show. A selector that uses one cannot be judged
// from the manifests.
var runtimeLabels = map[string]bool{
	"pod-template-hash":                        true,
	"controller-revision-hash":                 true,
	"statefulset.kubernetes.io/pod-name":       true,
	"apps.kubernetes.io/pod-index":             true,
	"batch.kubernetes.io/job-completion-index": true,
	"controller-uid":                           true,
	"batch.kubernetes.io/controller-uid":       true,
	"deployment":                               true, // OpenShift DeploymentConfig
	"deploymentconfig":                         true,
}

// ports lists the ports the pod's containers declare.
func (t *podTemplate) ports() (byName map[string]string, numbers map[string]bool) {
	byName, numbers = map[string]string{}, map[string]bool{}
	for _, list := range []string{"containers", "initContainers"} {
		for _, c := range getSlice(t.spec, list) {
			for _, p := range getSlice(asMap(c), "ports") {
				pm := asMap(p)
				num, isNum, ok := portValue(pm["containerPort"])
				if !ok || !isNum {
					continue
				}
				numbers[num] = true
				if name := getString(pm, "name"); name != "" {
					byName[name] = num
				}
			}
		}
	}
	return byName, numbers
}

// checkPodRefs follows every reference a pod makes to another object.
func (c *run) checkPodRefs(t *podTemplate) {
	o := t.owner
	podWhy := func(kind string) string { return "a pod can only use a " + kind + " in its own namespace" }

	for i, v := range getSlice(t.spec, "volumes") {
		vol := asMap(v)
		at := fmt.Sprintf("%s.volumes[%d]", t.path, i)
		if cm := asMap(vol["configMap"]); cm != nil {
			c.dataRef(o, kindConfigMap, at+".configMap.name", getString(cm, "name"), getBool(cm, "optional"), at+".configMap", cm)
		}
		if s := asMap(vol["secret"]); s != nil {
			c.dataRef(o, kindSecret, at+".secret.secretName", getString(s, "secretName"), getBool(s, "optional"), at+".secret", s)
		}
		for j, src := range getSlice(asMap(vol["projected"]), "sources") {
			sat := fmt.Sprintf("%s.projected.sources[%d]", at, j)
			if cm := asMap(asMap(src)["configMap"]); cm != nil {
				c.dataRef(o, kindConfigMap, sat+".configMap.name", getString(cm, "name"), getBool(cm, "optional"), sat+".configMap", cm)
			}
			if s := asMap(asMap(src)["secret"]); s != nil {
				c.dataRef(o, kindSecret, sat+".secret.name", getString(s, "name"), getBool(s, "optional"), sat+".secret", s)
			}
		}
		if claim := getString(asMap(vol["persistentVolumeClaim"]), "claimName"); claim != "" {
			_, l := c.resolve(nameRef{check: "pvc", from: o, field: at + ".persistentVolumeClaim.claimName", kind: kindPVC, ns: o.ns, name: claim, why: podWhy("PersistentVolumeClaim")})
			c.add(l)
		}
	}

	for _, list := range []string{"initContainers", "containers", "ephemeralContainers"} {
		for i, v := range getSlice(t.spec, list) {
			ctr := asMap(v)
			at := fmt.Sprintf("%s.%s[%d]", t.path, list, i)
			for j, e := range getSlice(ctr, "envFrom") {
				eat := fmt.Sprintf("%s.envFrom[%d]", at, j)
				if cm := asMap(asMap(e)["configMapRef"]); cm != nil {
					c.dataRef(o, kindConfigMap, eat+".configMapRef.name", getString(cm, "name"), getBool(cm, "optional"), "", nil)
				}
				if s := asMap(asMap(e)["secretRef"]); s != nil {
					c.dataRef(o, kindSecret, eat+".secretRef.name", getString(s, "name"), getBool(s, "optional"), "", nil)
				}
			}
			for j, e := range getSlice(ctr, "env") {
				from := asMap(asMap(e)["valueFrom"])
				eat := fmt.Sprintf("%s.env[%d].valueFrom", at, j)
				if cm := asMap(from["configMapKeyRef"]); cm != nil {
					c.keyRef(o, kindConfigMap, eat+".configMapKeyRef", getString(cm, "name"), getString(cm, "key"), getBool(cm, "optional"))
				}
				if s := asMap(from["secretKeyRef"]); s != nil {
					c.keyRef(o, kindSecret, eat+".secretKeyRef", getString(s, "name"), getString(s, "key"), getBool(s, "optional"))
				}
			}
		}
	}

	for i, v := range getSlice(t.spec, "imagePullSecrets") {
		if name := getString(asMap(v), "name"); name != "" {
			_, l := c.resolve(nameRef{check: "secret", from: o, field: fmt.Sprintf("%s.imagePullSecrets[%d].name", t.path, i), kind: kindSecret, ns: o.ns, name: name, why: podWhy("Secret")})
			c.add(l)
		}
	}

	for _, f := range []string{"serviceAccountName", "serviceAccount"} {
		if name := getString(t.spec, f); name != "" {
			_, l := c.resolve(nameRef{check: "service-account", from: o, field: t.path + "." + f, kind: kindServiceAccount, ns: o.ns, name: name, why: podWhy("ServiceAccount")})
			c.add(l)
			break
		}
	}
}

func checkName(kind gk) string {
	if kind == kindSecret {
		return "secret"
	}
	return "configmap"
}

// dataRef checks a reference to a whole ConfigMap or Secret, and then to
// each key picked out of it with "items".
func (c *run) dataRef(o *object, kind gk, field, name string, optional bool, itemsAt string, holder map[string]any) {
	if name == "" {
		return
	}
	target, l := c.resolve(nameRef{check: checkName(kind), from: o, field: field, kind: kind, ns: o.ns, name: name, optional: optional,
		why: "a pod can only use a " + kind.Kind + " in its own namespace"})
	c.add(l)
	if target == nil {
		return
	}
	for i, it := range getSlice(holder, "items") {
		if key := getString(asMap(it), "key"); key != "" {
			c.add(c.keyLink(o, target, fmt.Sprintf("%s.items[%d].key", itemsAt, i), key, optional))
		}
	}
}

// keyRef checks a reference to one key of a ConfigMap or Secret.
func (c *run) keyRef(o *object, kind gk, at, name, key string, optional bool) {
	if name == "" {
		return
	}
	target, l := c.resolve(nameRef{check: checkName(kind), from: o, field: at + ".name", kind: kind, ns: o.ns, name: name, optional: optional,
		why: "a pod can only use a " + kind.Kind + " in its own namespace"})
	if target == nil || key == "" {
		l.To.Key = key
		c.add(l)
		return
	}
	c.add(c.keyLink(o, target, at+".key", key, optional))
}

// keysOf lists the keys of a ConfigMap or Secret. Only the names are read.
// The values are never copied anywhere.
func keysOf(o *object) []string {
	keys := map[string]bool{}
	for _, f := range []string{"data", "binaryData", "stringData"} {
		for k := range getMap(o.res.Object, f) {
			keys[k] = true
		}
	}
	// The cluster fills these in for a token Secret.
	if o.gk == kindSecret && getString(o.res.Object, "type") == "kubernetes.io/service-account-token" {
		for _, k := range []string{"token", "ca.crt", "namespace"} {
			keys[k] = true
		}
	}
	return sortedKeys(keys)
}

// keyLink checks that a ConfigMap or Secret that was found has a key.
func (c *run) keyLink(o, target *object, field, key string, optional bool) links.Link {
	kind := target.gk.Kind
	l := links.Link{Check: checkName(target.gk), From: o.ref, Field: field,
		To: links.Target{Kind: kind, Namespace: target.ns, Name: target.name, Key: key, Source: target.ref.Source}}
	keys := keysOf(target)
	l.Expected = fmt.Sprintf("%s to have the key %q", target.ref, key)
	l.Candidates = limit(keys, 30)
	for _, k := range keys {
		if k == key {
			l.Status = links.StatusOK
			l.Message = fmt.Sprintf("%s uses key %q of %s, which has it.", o.ref, key, target.ref)
			return l
		}
	}
	// An optional key that is absent never stops the pod, whether the
	// object has other keys or none.
	if optional {
		l.Status, l.Reason = links.StatusSkipped, links.ReasonOptional
		l.Found = "keys: " + joinList(limit(keys, 30))
		if len(keys) == 0 {
			l.Found = "no keys at all"
		}
		l.Message = fmt.Sprintf("%s uses key %q of %s, which does not have it. The reference is marked optional, so the pod starts without it.", o.ref, key, target.ref)
		return l
	}
	// An object with no keys at all is usually a placeholder that something
	// in the cluster fills in, such as a CA bundle injector.
	if len(keys) == 0 {
		l.Status, l.Reason = links.StatusUnresolved, links.ReasonDetailsUnknown
		l.Found = "no keys at all"
		l.Message = fmt.Sprintf("%s uses key %q of %s, which lists no keys here. Something in the cluster may fill it in, so this cannot be decided.", o.ref, key, target.ref)
		l.Fix = fmt.Sprintf("If the %s is filled in by the cluster, pass a copy with its keys in clusterObjects. Otherwise add the key.", kind)
		return l
	}
	l.Found = "keys: " + joinList(limit(keys, 30))
	l.Status, l.Reason = links.StatusBroken, links.ReasonKeyMissing
	l.Message = fmt.Sprintf("%s uses key %q of %s, which does not have that key. The pod will not start.", o.ref, key, target.ref)
	l.Fix = fmt.Sprintf("Use a key the %s has, or add %q to it.", kind, key)
	if n, ok := nearest(key, keys); ok {
		l.ClosestMatch = fmt.Sprintf("key %s of %s", n, target.ref)
	}
	return l
}

// checkStatefulSetService checks the Service a StatefulSet names for its
// pods' network identity.
func (c *run) checkStatefulSetService(o *object) {
	name := getString(o.spec(), "serviceName")
	if name == "" {
		return
	}
	_, l := c.resolve(nameRef{check: "statefulset-service", from: o, field: "spec.serviceName", kind: kindService, ns: o.ns, name: name,
		why: "a StatefulSet's Service must be in its own namespace"})
	c.add(l)
}
