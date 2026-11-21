package checks

import (
	"fmt"
	"path"
	"strings"
)

// Check describes one check. The name is what a link carries in its
// "check" field.
type Check struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Kinds lists the kinds of resource the check reads its references
	// from.
	Kinds []string `json:"kinds"`
	// Decides says which outcomes are certain and which are left open.
	Decides string `json:"decides"`
}

const workloads = "Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, ReplicationController, Job, CronJob, DeploymentConfig, and any kind with a pod template at spec.template"

// registry is every check, in the order they are listed. A link may only
// carry a name from this table.
var registry = []Check{
	{"service-selector", "A Service's selector matches the pod template of a workload.", []string{"Service"},
		"Unresolved when it matches nothing: matching pods may be in the cluster."},
	{"service-port", "A Service's target port is a container port of the pods it selects.", []string{"Service"},
		"Broken when a named port is declared by none of the selected pods. Unresolved when a number is not declared, because declaring ports is optional."},
	{"workload-selector", "A workload's selector matches its own pod template labels.", []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Job", "DeploymentConfig"},
		"Broken when it does not match. Both ends are in one resource, so this is always certain."},
	{"configmap", "A pod's ConfigMap exists and has the keys the pod uses.", []string{workloads},
		"Broken when the ConfigMap is in the bundle without the key. Unresolved when the ConfigMap is not in the bundle."},
	{"secret", "A pod's Secret exists and has the keys the pod uses. Covers image pull secrets.", []string{workloads},
		"Broken when the Secret is in the bundle without the key. Unresolved when the Secret is not in the bundle."},
	{"pvc", "A pod's PersistentVolumeClaim exists.", []string{workloads},
		"Unresolved when the claim is not in the bundle."},
	{"service-account", "A pod's ServiceAccount exists.", []string{workloads},
		"Unresolved when the ServiceAccount is not in the bundle."},
	{"statefulset-service", "The Service a StatefulSet names in spec.serviceName exists.", []string{"StatefulSet"},
		"Unresolved when the Service is not in the bundle."},
	{"ingress-backend", "An Ingress backend's Service exists and exposes the port.", []string{"Ingress"},
		"Broken when the Service is in the bundle without the port. Unresolved when the Service is not in the bundle."},
	{"ingress-tls", "An Ingress's TLS Secret exists.", []string{"Ingress"},
		"Unresolved when the Secret is not in the bundle."},
	{"route-backend", "A Route's Service exists and has the port.", []string{"Route"},
		"Broken when the Service is in the bundle without the port. Unresolved when the Service is not in the bundle."},
	{"hpa-target", "The workload a HorizontalPodAutoscaler scales exists.", []string{"HorizontalPodAutoscaler"},
		"Unresolved when the workload is not in the bundle."},
	{"pdb-selector", "A PodDisruptionBudget's selector matches the pod template of a workload.", []string{"PodDisruptionBudget"},
		"Unresolved when it matches nothing."},
	{"networkpolicy-selector", "A NetworkPolicy's pod selector matches the pod template of a workload.", []string{"NetworkPolicy"},
		"Unresolved when it matches nothing."},
	{"binding-role", "The role a RoleBinding or ClusterRoleBinding refers to exists.", []string{"RoleBinding", "ClusterRoleBinding"},
		"Broken when a ClusterRoleBinding refers to a Role. Unresolved when the role is not in the bundle."},
	{"binding-subject", "A ServiceAccount named as a subject of a binding exists.", []string{"RoleBinding", "ClusterRoleBinding"},
		"Unresolved when the ServiceAccount is not in the bundle."},
}

// List returns every check.
func List() []Check { return append([]Check(nil), registry...) }

// Names returns the name of every check, in listing order.
func Names() []string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return names
}

var registered = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range registry {
		if m[c.Name] {
			panic("check registered twice: " + c.Name)
		}
		m[c.Name] = true
	}
	return m
}()

// runner is a function that produces the links of one or more checks. Some
// checks share a function because they read the same part of a resource:
// a Service's ports are compared with the pods its selector found.
type runner struct {
	// names are the checks the function produces. The first also names
	// the function if it fails.
	names []string
	// kinds limits the function to these kinds. Empty means every kind.
	kinds []gk
	fn    func(c *run, o *object)
}

var runners = []runner{
	{[]string{"service-selector", "service-port"}, []gk{kindService}, (*run).checkService},
	{[]string{"ingress-backend", "ingress-tls"}, []gk{kindIngress, kindOldIngress}, (*run).checkIngress},
	{[]string{"route-backend"}, []gk{kindRoute}, (*run).checkRoute},
	{[]string{"hpa-target"}, []gk{kindHPA}, (*run).checkHPA},
	{[]string{"pdb-selector"}, []gk{kindPDB}, func(c *run, o *object) { c.checkPodSelector(o, "pdb-selector", "selector") }},
	{[]string{"networkpolicy-selector"}, []gk{kindNetworkPolicy}, func(c *run, o *object) { c.checkPodSelector(o, "networkpolicy-selector", "podSelector") }},
	{[]string{"binding-role", "binding-subject"}, []gk{kindRoleBinding, kindCRB}, (*run).checkBinding},
	{[]string{"workload-selector"}, nil, (*run).checkWorkloadSelector},
	{[]string{"statefulset-service"}, []gk{kindStatefulSet}, (*run).checkStatefulSetService},
	{[]string{"configmap", "secret", "pvc", "service-account"}, nil, func(c *run, o *object) {
		for _, t := range templatesOf(o) {
			c.checkPodRefs(t)
		}
	}},
}

func init() {
	// Every check has a function, and every function a check.
	produced := map[string]bool{}
	for _, r := range runners {
		for _, n := range r.names {
			if !registered[n] {
				panic("runner produces an unregistered check: " + n)
			}
			produced[n] = true
		}
	}
	for n := range registered {
		if !produced[n] {
			panic("check has no runner: " + n)
		}
	}
}

func (r runner) appliesTo(k gk) bool {
	if len(r.kinds) == 0 {
		return true
	}
	for _, want := range r.kinds {
		if want == k {
			return true
		}
	}
	return false
}

// Selection is the set of checks to run.
type Selection struct {
	enabled map[string]bool
}

// Enabled reports whether a check is to run.
func (s Selection) Enabled(name string) bool { return s.enabled[name] }

// Run lists the checks that are on, in listing order.
func (s Selection) Run() []string { return s.pick(true) }

// Skipped lists the checks that are off, in listing order.
func (s Selection) Skipped() []string { return s.pick(false) }

func (s Selection) pick(on bool) []string {
	out := []string{}
	for _, n := range Names() {
		if s.enabled[n] == on {
			out = append(out, n)
		}
	}
	return out
}

// Select works out which checks to run. only names the checks to run, and
// an empty list means all of them. skip names checks to leave out. Both
// accept * as a wildcard, as in "ingress-*". An entry that matches no check
// is an error, so a typing mistake cannot shrink the check unnoticed.
func Select(only, skip []string) (Selection, error) {
	match := func(patterns []string) (map[string]bool, error) {
		hit := map[string]bool{}
		for _, p := range patterns {
			p = strings.TrimSpace(p)
			found := false
			for _, n := range Names() {
				if ok, err := path.Match(p, n); err != nil {
					return nil, fmt.Errorf("%q is not a valid check pattern", p)
				} else if ok {
					hit[n], found = true, true
				}
			}
			if !found {
				return nil, fmt.Errorf("no check is named %q; the checks are: %s", p, strings.Join(Names(), ", "))
			}
		}
		return hit, nil
	}
	wanted, err := match(only)
	if err != nil {
		return Selection{}, err
	}
	skipped, err := match(skip)
	if err != nil {
		return Selection{}, err
	}
	s := Selection{enabled: map[string]bool{}}
	for _, n := range Names() {
		s.enabled[n] = (len(only) == 0 || wanted[n]) && !skipped[n]
	}
	if len(s.Run()) == 0 {
		return Selection{}, fmt.Errorf("the selection leaves no check to run")
	}
	return s, nil
}
