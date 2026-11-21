// Package checks finds the references between the resources of a bundle
// and says whether each one resolves. It reads manifests only. It never
// contacts a cluster.
//
// The rule behind every status: never claim more than can be known. A
// target that is not in the bundle may exist in the cluster, so it is
// "unresolved", not "broken", unless the caller says the bundle is complete.
package checks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
	"github.com/cosmosdevops/kube-refs-mcp/internal/manifests"
)

// Request is one bundle to check.
type Request struct {
	// Resources is the bundle.
	Resources []manifests.Resource
	// ClusterObjects are objects that already exist in the cluster. They
	// can be the target of a link, but their own references are not
	// checked.
	ClusterObjects []manifests.Resource
	// Namespace is used for namespaced resources that do not set one.
	Namespace string
	// BundleComplete says the bundle and the cluster objects hold
	// everything, so a target that is missing counts as broken.
	BundleComplete bool
	// IncludeAll also lists the links that are ok or skipped.
	IncludeAll   bool
	SkippedFiles []links.SkippedFile
	// Checks names the checks to run. Empty means all of them.
	Checks []string
	// SkipChecks names checks to leave out. See Select.
	SkipChecks []string
}

// Result is a finished check.
type Result struct {
	Report *links.Report
	// Links holds every link, including the ones that are fine.
	Links []links.Link
	// Resources is the bundle as checked, with the namespace applied.
	Resources []manifests.Resource
}

// gk is an API group and kind. Kinds are compared with their group, so a
// custom resource that shares a name with a built-in kind is kept apart.
type gk struct{ Group, Kind string }

var (
	kindPod            = gk{"", "Pod"}
	kindService        = gk{"", "Service"}
	kindConfigMap      = gk{"", "ConfigMap"}
	kindSecret         = gk{"", "Secret"}
	kindPVC            = gk{"", "PersistentVolumeClaim"}
	kindServiceAccount = gk{"", "ServiceAccount"}
	kindRC             = gk{"", "ReplicationController"}
	kindDeployment     = gk{"apps", "Deployment"}
	kindReplicaSet     = gk{"apps", "ReplicaSet"}
	kindStatefulSet    = gk{"apps", "StatefulSet"}
	kindDaemonSet      = gk{"apps", "DaemonSet"}
	kindJob            = gk{"batch", "Job"}
	kindCronJob        = gk{"batch", "CronJob"}
	kindDC             = gk{"apps.openshift.io", "DeploymentConfig"}
	kindIngress        = gk{"networking.k8s.io", "Ingress"}
	kindOldIngress     = gk{"extensions", "Ingress"}
	kindNetworkPolicy  = gk{"networking.k8s.io", "NetworkPolicy"}
	kindRoute          = gk{"route.openshift.io", "Route"}
	kindHPA            = gk{"autoscaling", "HorizontalPodAutoscaler"}
	kindPDB            = gk{"policy", "PodDisruptionBudget"}
	kindRole           = gk{"rbac.authorization.k8s.io", "Role"}
	kindClusterRole    = gk{"rbac.authorization.k8s.io", "ClusterRole"}
	kindRoleBinding    = gk{"rbac.authorization.k8s.io", "RoleBinding"}
	kindCRB            = gk{"rbac.authorization.k8s.io", "ClusterRoleBinding"}
)

func groupOf(apiVersion string) string {
	if i := strings.Index(apiVersion, "/"); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

// object is one resource in the index.
type object struct {
	res  manifests.Resource
	gk   gk
	ns   string
	name string
	// cluster is true for an object supplied as context.
	cluster bool
	ref     links.Ref
}

func (o *object) spec() map[string]any { return getMap(o.res.Object, "spec") }

// run holds the state of one check request.
type run struct {
	req     Request
	sel     Selection
	objects []*object
	// byName finds an object by kind, namespace and name.
	byName    map[gk]map[string]map[string]*object
	templates []*podTemplate
	links     []links.Link
	notes     []string
}

const clusterSource = "clusterObjects"

// These sentences end the message of a link whose target is missing. They
// are dropped when the caller says the bundle is complete.
const (
	mayExist     = " It may already exist in the cluster."
	podsMayExist = " Matching pods may already exist in the cluster."
)

// Run checks a bundle. It returns an error only when the request cannot
// be checked at all. A check that fails part-way becomes an error link.
func Run(ctx context.Context, req Request) (*Result, error) {
	sel, err := Select(req.Checks, req.SkipChecks)
	if err != nil {
		return nil, err
	}
	c := &run{req: req, sel: sel, byName: map[gk]map[string]map[string]*object{}}

	bundle, defaulted := manifests.ApplyDefaultNamespace(req.Resources, req.Namespace)
	cluster, _ := manifests.ApplyDefaultNamespace(req.ClusterObjects, req.Namespace)
	if defaulted > 0 {
		c.notef("%d resource(s) set no namespace and were treated as being in %q.", defaulted, req.Namespace)
	}
	unset, set := 0, 0
	for _, r := range bundle {
		o := c.index(r, false)
		switch {
		case manifests.IsClusterScoped(o.gk.Kind):
		case o.ns == "":
			unset++
		default:
			set++
		}
	}
	for _, r := range cluster {
		r.Source, r.File = clusterSource, clusterSource
		c.index(r, true)
	}
	// Worth saying only when the bundle mixes the two: a bundle that never
	// names a namespace is deployed to one place and needs no remark.
	if unset > 0 && set > 0 {
		c.notef("%d resource(s) set no namespace and %d do. Those with none were treated as being together, apart from every named namespace. Pass a namespace to say where they go.", unset, set)
	}
	if len(cluster) > 0 {
		c.notef("%d object(s) were supplied as existing in the cluster. They were taken on trust and not checked.", len(cluster))
	}
	if req.BundleComplete {
		c.notef("The bundle was said to be complete, so a target that is missing counts as broken.")
	}
	if off := sel.Skipped(); len(off) > 0 {
		c.notef("%d check(s) were turned off and did not run: %s. The verdict covers only the checks that ran.", len(off), strings.Join(off, ", "))
	}

	for _, o := range c.objects {
		c.templates = append(c.templates, templatesOf(o)...)
	}
	for _, o := range c.objects {
		if o.cluster {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("the check ran out of time after %d of %d resources: %w", len(c.links), len(c.objects), err)
		}
		c.checkObject(o)
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	rep := &links.Report{CheckID: id, BundleComplete: req.BundleComplete, ChecksRun: sel.Run(), ChecksSkipped: sel.Skipped(), SkippedFiles: req.SkippedFiles, Notes: c.notes, Resources: []links.Ref{}, Findings: []links.Link{}}
	for _, o := range c.objects {
		if !o.cluster {
			rep.Resources = append(rep.Resources, o.ref)
		}
	}
	rep.Summary.Resources = len(rep.Resources)
	for i := range c.links {
		c.links[i].ID = fmt.Sprintf("f-%s-%03d", id, i+1)
		if req.IncludeAll || c.links[i].Status.Actionable() {
			rep.Findings = append(rep.Findings, c.links[i])
		}
	}
	links.Aggregate(rep, c.links)
	return &Result{Report: rep, Links: c.links, Resources: bundle}, nil
}

func newID() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (c *run) notef(format string, args ...any) {
	c.notes = append(c.notes, fmt.Sprintf(format, args...))
}

// index adds a resource to the lookup tables.
func (c *run) index(r manifests.Resource, cluster bool) *object {
	o := &object{res: r, gk: gk{groupOf(r.APIVersion()), r.Kind()}, ns: r.Namespace(), name: r.Name(), cluster: cluster, ref: r.Ref()}
	if manifests.IsClusterScoped(o.gk.Kind) {
		o.ns = ""
	}
	byNS := c.byName[o.gk]
	if byNS == nil {
		byNS = map[string]map[string]*object{}
		c.byName[o.gk] = byNS
	}
	if byNS[o.ns] == nil {
		byNS[o.ns] = map[string]*object{}
	}
	if old := byNS[o.ns][o.name]; old != nil && !old.cluster && !cluster {
		c.notef("%s is in the bundle more than once (%s and %s). The later one was used.", o.ref, old.ref.Source, o.ref.Source)
	}
	// An object in the bundle wins over the same object given as context.
	if old := byNS[o.ns][o.name]; old == nil || old.cluster || !cluster {
		byNS[o.ns][o.name] = o
	}
	c.objects = append(c.objects, o)
	return o
}

// checkObject runs every check that applies to one resource and is turned
// on. A check that panics becomes an error link for that resource, never a
// pass.
func (c *run) checkObject(o *object) {
	for _, r := range runners {
		if !r.appliesTo(o.gk) || !c.anyEnabled(r.names) {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					c.add(links.Link{Check: r.names[0], Status: links.StatusError, Reason: links.ReasonCheckFailed, From: o.ref,
						Message: fmt.Sprintf("The %s check failed on %s: %v. This is a fault in kube-refs-mcp, not in the manifest.", r.names[0], o.ref, p)})
				}
			}()
			r.fn(c, o)
		}()
	}
}

func (c *run) anyEnabled(names []string) bool {
	for _, n := range names {
		if c.sel.Enabled(n) {
			return true
		}
	}
	return false
}

// add records a link. Every link passes through here, so this is the one
// place where a check that is turned off is dropped, and where "the bundle
// is complete" turns a missing target into a broken link.
func (c *run) add(l links.Link) {
	if !registered[l.Check] {
		panic(fmt.Sprintf("link with the unregistered check name %q", l.Check))
	}
	// A check that is turned off leaves no links. A failure is kept even
	// so: it is never hidden.
	if !c.sel.Enabled(l.Check) && l.Status != links.StatusError {
		return
	}
	if c.req.BundleComplete && l.Status == links.StatusUnresolved && l.Reason == links.ReasonNotInBundle {
		l.Status = links.StatusBroken
		l.Message = strings.TrimSuffix(strings.TrimSuffix(l.Message, mayExist), podsMayExist) + " The bundle was said to be complete, so this is broken."
	}
	c.links = append(c.links, l)
}

// mixedNamespaces reports whether exactly one of two namespaces is unset.
// The two might then still be the same namespace once applied, so a lookup
// across them proves nothing.
func mixedNamespaces(a, b string) bool { return (a == "") != (b == "") }

// nameRef is a reference to one object by name.
type nameRef struct {
	check string
	from  *object
	field string
	kind  gk
	// ns is the namespace the target must be in. Empty for a
	// cluster-scoped kind.
	ns       string
	name     string
	optional bool
	// why says, for the message, why the target must be in ns, for example
	// "a pod can only use Secrets in its own namespace".
	why string
}

// resolve looks the target up and returns the link to record. The caller
// may refine the link (a key or a port) before adding it. target is nil
// unless the link is ok.
func (c *run) resolve(r nameRef) (target *object, l links.Link) {
	clusterScoped := manifests.IsClusterScoped(r.kind.Kind)
	l = links.Link{Check: r.check, From: r.from.ref, Field: r.field, To: links.Target{Kind: r.kind.Kind, Namespace: r.ns, Name: r.name}}
	where := ""
	if !clusterScoped && r.ns != "" {
		where = fmt.Sprintf(" in namespace %q", r.ns)
	}
	if t := c.byName[r.kind][r.ns][r.name]; t != nil {
		l.Status = links.StatusOK
		l.To.Source = t.ref.Source
		l.Message = fmt.Sprintf("%s refers to %s %q%s, which is in the bundle.", r.from.ref, r.kind.Kind, r.name, where)
		if t.cluster {
			l.Message = fmt.Sprintf("%s refers to %s %q%s, which was supplied as existing in the cluster.", r.from.ref, r.kind.Kind, r.name, where)
		}
		return t, l
	}
	if clusterProvided(r.kind, r.name) {
		l.Status, l.Reason = links.StatusSkipped, links.ReasonClusterProvided
		l.Message = fmt.Sprintf("%s refers to %s %q%s. The cluster creates it, so it does not need to be in the bundle.", r.from.ref, r.kind.Kind, r.name, where)
		return nil, l
	}

	// Not found. Collect what does exist, for the hint and the explanation.
	var sameNS, elsewhere []string
	for ns, byName := range c.byName[r.kind] {
		for name := range byName {
			if ns == r.ns {
				sameNS = append(sameNS, name)
			} else if name == r.name {
				elsewhere = append(elsewhere, ns)
			}
		}
	}
	sort.Strings(sameNS)
	sort.Strings(elsewhere)
	l.Expected = fmt.Sprintf("%s %s named %q%s", article(r.kind.Kind), r.kind.Kind, r.name, where)
	l.Found = fmt.Sprintf("no %s with that name%s in the bundle", r.kind.Kind, where)
	for _, n := range limit(sameNS, 20) {
		l.Candidates = append(l.Candidates, r.kind.Kind+" "+n+where)
	}

	if r.optional {
		l.Status, l.Reason = links.StatusSkipped, links.ReasonOptional
		l.Message = fmt.Sprintf("%s refers to %s %q%s, which is not in the bundle. The reference is marked optional, so the pod starts without it.", r.from.ref, r.kind.Kind, r.name, where)
		return nil, l
	}

	l.Status, l.Reason = links.StatusUnresolved, links.ReasonNotInBundle
	l.Message = fmt.Sprintf("%s refers to %s %q%s, which is not in the bundle.%s", r.from.ref, r.kind.Kind, r.name, where, mayExist)
	l.Fix = fmt.Sprintf("Add the %s to the bundle or correct the name in %s. If it already exists in the cluster, pass it in clusterObjects.", r.kind.Kind, r.field)

	// The same name under an unset namespace, or seen from one: the two may
	// be the same place. That is a gap in what we were told, not in the
	// bundle, so bundleComplete does not turn it into broken.
	if !clusterScoped {
		for _, ns := range elsewhere {
			if mixedNamespaces(ns, r.ns) {
				l.Reason = links.ReasonNamespaceUnknown
				l.Message = fmt.Sprintf("%s refers to %s %q. The bundle has one, but only one of the two sets a namespace, so it is not known whether they end up together.", r.from.ref, r.kind.Kind, r.name)
				l.Fix = "Set metadata.namespace on both resources, or pass the namespace the bundle is deployed to."
				return nil, l
			}
		}
	}
	switch {
	case len(elsewhere) > 0:
		why := r.why
		if why == "" {
			why = "the reference only looks in its own namespace"
		}
		l.ClosestMatch = fmt.Sprintf("%s %s exists in namespace %s, but %s", r.kind.Kind, r.name, strings.Join(limit(elsewhere, 5), ", "), why)
	default:
		if n, ok := nearest(r.name, sameNS); ok {
			l.ClosestMatch = fmt.Sprintf("%s %s%s", r.kind.Kind, n, where)
		}
	}
	return nil, l
}

// clusterProvided reports whether every cluster creates the object itself.
// The list is short on purpose: only objects that upstream Kubernetes
// always creates.
func clusterProvided(k gk, name string) bool {
	switch k {
	case kindServiceAccount:
		return name == "default"
	case kindConfigMap:
		return name == "kube-root-ca.crt"
	case kindClusterRole:
		switch name {
		case "cluster-admin", "admin", "edit", "view":
			return true
		}
		return strings.HasPrefix(name, "system:")
	}
	return false
}
