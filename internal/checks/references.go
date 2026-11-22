package checks

import (
	"fmt"
	"sort"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// servicePorts describes the ports of a Service for messages, and indexes
// them by number and by name.
type servicePorts struct {
	numbers map[string]bool
	names   map[string]bool
	// targets holds each port's target port, as text, and whether every
	// one of them is a number.
	targets     map[string]bool
	allNumeric  bool
	description []string
}

func portsOfService(svc *object) servicePorts {
	sp := servicePorts{numbers: map[string]bool{}, names: map[string]bool{}, targets: map[string]bool{}, allNumeric: true}
	for _, p := range getSlice(svc.spec(), "ports") {
		pm := asMap(p)
		num, isNum, ok := portValue(pm["port"])
		if !ok || !isNum {
			continue
		}
		sp.numbers[num] = true
		text := num
		if name := getString(pm, "name"); name != "" {
			sp.names[name] = true
			text = fmt.Sprintf("%s (%s)", num, name)
		}
		sp.description = append(sp.description, text)
		target, targetIsNum, ok := portValue(pm["targetPort"])
		if !ok {
			target, targetIsNum = num, true
		}
		sp.targets[target] = true
		if !targetIsNum {
			sp.allNumeric = false
		}
	}
	return sp
}

// backend is a reference from an Ingress or a Route to a Service.
type backend struct {
	check, field, name string
	// port is the Service port asked for, by number or name. Empty if none.
	port      string
	portField string
}

// checkBackend checks that the Service exists and exposes the port.
func (c *run) checkBackend(o *object, b backend) {
	svc, l := c.resolve(nameRef{check: b.check, from: o, field: b.field, kind: kindService, ns: o.ns, name: b.name,
		why: fmt.Sprintf("%s %s can only send to a Service in its own namespace", article(o.gk.Kind), o.gk.Kind)})
	if svc == nil || b.port == "" {
		l.To.Port = b.port
		c.add(l)
		return
	}
	c.add(c.portLink(o, svc, l, b.port, b.portField))
}

// portLink compares a port a resource asks for with the ports a Service
// exposes. l is the link that found the Service.
func (c *run) portLink(o, svc *object, l links.Link, port, portField string) links.Link {
	sp := portsOfService(svc)
	l.Field, l.To.Port = portField, port
	l.Expected = fmt.Sprintf("%s to expose port %s", svc.ref, port)
	l.Candidates = sp.description
	l.Found = "service ports: " + joinList(sp.description)
	switch {
	case sp.numbers[port] || sp.names[port]:
		l.Message = fmt.Sprintf("%s sends to port %s of %s, which exposes it.", o.ref, port, svc.ref)
	case len(sp.description) == 0 && svc.cluster:
		l.Status, l.Reason = links.StatusUnresolved, links.ReasonDetailsUnknown
		l.Found = "no ports listed"
		l.Message = fmt.Sprintf("%s sends to port %s of %s, which was supplied without its ports, so this cannot be decided.", o.ref, port, svc.ref)
		l.Fix = "Include spec.ports in the copy of the Service passed in clusterObjects."
	default:
		l.Status, l.Reason = links.StatusBroken, links.ReasonPortMissing
		l.Message = fmt.Sprintf("%s sends to port %s of %s, which does not expose that port.", o.ref, port, svc.ref)
		l.Fix = fmt.Sprintf("Change %s to a port the Service exposes, or add the port to the Service.", portField)
		if len(sp.description) > 0 {
			l.ClosestMatch = fmt.Sprintf("%s exposes %s", svc.ref, joinList(sp.description))
		}
	}
	return l
}

// checkIngress checks every backend of an Ingress, and its TLS Secrets.
func (c *run) checkIngress(o *object) {
	spec := o.spec()
	one := func(at string, be map[string]any) {
		if be == nil {
			return
		}
		if svc := asMap(be["service"]); svc != nil {
			b := backend{check: "ingress-backend", field: at + ".service.name", name: getString(svc, "name")}
			port := asMap(svc["port"])
			if p, _, ok := portValue(port["number"]); ok {
				b.port, b.portField = p, at+".service.port.number"
			} else if p := getString(port, "name"); p != "" {
				b.port, b.portField = p, at+".service.port.name"
			}
			if b.name != "" {
				c.checkBackend(o, b)
			}
			return
		}
		// The older form, from before networking.k8s.io/v1.
		if name := getString(be, "serviceName"); name != "" {
			b := backend{check: "ingress-backend", field: at + ".serviceName", name: name}
			if p, _, ok := portValue(be["servicePort"]); ok {
				b.port, b.portField = p, at+".servicePort"
			}
			c.checkBackend(o, b)
		}
	}
	one("spec.defaultBackend", asMap(spec["defaultBackend"]))
	one("spec.backend", asMap(spec["backend"]))
	for i, r := range getSlice(spec, "rules") {
		for j, p := range getSlice(getMap(asMap(r), "http"), "paths") {
			one(fmt.Sprintf("spec.rules[%d].http.paths[%d].backend", i, j), asMap(asMap(p)["backend"]))
		}
	}
	for i, t := range getSlice(spec, "tls") {
		if name := getString(asMap(t), "secretName"); name != "" {
			_, l := c.resolve(nameRef{check: "ingress-tls", from: o, field: fmt.Sprintf("spec.tls[%d].secretName", i), kind: kindSecret, ns: o.ns, name: name,
				why: "an Ingress can only use a Secret in its own namespace"})
			c.add(l)
		}
	}
}

// checkRoute checks the Services an OpenShift Route sends to.
//
// A Route's port.targetPort is matched by the router against the endpoints
// of the Service: a name is a Service port name, and a number is the port
// on the pods, which is the Service's target port.
func (c *run) checkRoute(o *object) {
	spec := o.spec()
	port, portIsNum, hasPort := portValue(getMap(spec, "port")["targetPort"])
	one := func(at string, to map[string]any) {
		name := getString(to, "name")
		if name == "" {
			return
		}
		if kind := getString(to, "kind"); kind != "" && kind != "Service" {
			c.add(links.Link{Check: "route-backend", Status: links.StatusSkipped, Reason: links.ReasonNotApplicable, From: o.ref, Field: at + ".kind",
				To:      links.Target{Kind: kind, Namespace: o.ns, Name: name},
				Message: fmt.Sprintf("%s sends to a %s, not a Service, so it was not followed.", o.ref, kind)})
			return
		}
		svc, l := c.resolve(nameRef{check: "route-backend", from: o, field: at + ".name", kind: kindService, ns: o.ns, name: name,
			why: "a Route can only send to a Service in its own namespace"})
		if svc == nil || !hasPort {
			if hasPort {
				l.To.Port = port
			}
			c.add(l)
			return
		}
		sp := portsOfService(svc)
		l.Field, l.To.Port = "spec.port.targetPort", port
		l.Candidates = sp.description
		l.Found = "service ports: " + joinList(sp.description)
		l.Expected = fmt.Sprintf("%s to have a port named %q, or a target port with that number", svc.ref, port)
		switch {
		case sp.names[port] || (portIsNum && sp.targets[port]):
			l.Message = fmt.Sprintf("%s sends to port %s of %s, which has it.", o.ref, port, svc.ref)
		case sp.targets[port]:
			// The name is one of the Service's target ports. That is a name
			// on the pods, not a Service port name. Whether the router
			// accepts it has not been checked against a running router.
			l.Status, l.Reason = links.StatusUnresolved, links.ReasonPortNotDeclared
			l.Message = fmt.Sprintf("%s sends to the port named %q of %s. That is the name of a target port on the pods, not of a Service port. Whether the router accepts it has not been checked against a running router.", o.ref, port, svc.ref)
			l.Fix = "Set spec.port.targetPort to the name of a Service port, or to the Service's target port number."
		case len(sp.description) == 0 && svc.cluster:
			l.Status, l.Reason = links.StatusUnresolved, links.ReasonDetailsUnknown
			l.Found = "no ports listed"
			l.Message = fmt.Sprintf("%s sends to port %s of %s, which was supplied without its ports, so this cannot be decided.", o.ref, port, svc.ref)
			l.Fix = "Include spec.ports in the copy of the Service passed in clusterObjects."
		case portIsNum && !sp.allNumeric:
			// The number is compared with the port on the pods. A Service
			// that names its target port hides that number.
			l.Status, l.Reason = links.StatusSkipped, links.ReasonDetailsUnknown
			l.Message = fmt.Sprintf("%s sends to pod port %s through %s, which names its target ports. The numbers behind those names are on the pods, so this was not compared.", o.ref, port, svc.ref)
		case portIsNum:
			// How the router treats a number is taken from its documentation
			// and has not been tested against a running router. That is not
			// enough to call the link broken.
			l.Status, l.Reason = links.StatusUnresolved, links.ReasonPortNotDeclared
			l.Message = fmt.Sprintf("%s sends to pod port %s through %s, which has no target port with that number. The router is expected to find no endpoints, but this rule has not been checked against a running router.", o.ref, port, svc.ref)
			l.Fix = "Set spec.port.targetPort to the name of a Service port, or to the Service's target port number."
			if len(sp.description) > 0 {
				l.ClosestMatch = fmt.Sprintf("%s exposes %s", svc.ref, joinList(sp.description))
			}
		default:
			l.Status, l.Reason = links.StatusBroken, links.ReasonPortMissing
			l.Message = fmt.Sprintf("%s sends to the port named %q of %s, which has no port with that name. The router would find no endpoints.", o.ref, port, svc.ref)
			l.Fix = "Set spec.port.targetPort to the name of a Service port, or to the Service's target port number."
			if n, ok := nearest(port, sortedKeys(sp.names)); ok {
				l.ClosestMatch = fmt.Sprintf("port %s of %s", n, svc.ref)
			} else if len(sp.description) > 0 {
				l.ClosestMatch = fmt.Sprintf("%s exposes %s", svc.ref, joinList(sp.description))
			}
		}
		c.add(l)
	}
	one("spec.to", asMap(spec["to"]))
	for i, b := range getSlice(spec, "alternateBackends") {
		one(fmt.Sprintf("spec.alternateBackends[%d]", i), asMap(b))
	}
}

// checkHPA checks that the workload an autoscaler scales is there. The
// target may be any kind, so it is looked up by group and kind as written.
func (c *run) checkHPA(o *object) {
	ref := getMap(o.spec(), "scaleTargetRef")
	kind, name := getString(ref, "kind"), getString(ref, "name")
	if kind == "" || name == "" {
		return
	}
	target := gk{groupOf(getString(ref, "apiVersion")), kind}
	_, l := c.resolve(nameRef{check: "hpa-target", from: o, field: "spec.scaleTargetRef.name", kind: target, ns: o.ns, name: name,
		why: "an autoscaler can only scale a workload in its own namespace"})
	if l.Status == links.StatusUnresolved {
		// The same name under another kind is the likeliest slip, so it
		// replaces a hint that only found a similar name. Kinds are tried in
		// a fixed order, so the hint does not change from run to run.
		var others []gk
		for other, byNS := range c.byName {
			if t := byNS[o.ns][name]; t != nil && other != target && len(templatesOf(t)) > 0 {
				others = append(others, other)
			}
		}
		sort.Slice(others, func(i, j int) bool {
			return others[i].Group+"/"+others[i].Kind < others[j].Group+"/"+others[j].Kind
		})
		if len(others) > 0 {
			t := c.byName[others[0]][o.ns][name]
			l.ClosestMatch = fmt.Sprintf("%s (apiVersion %s) has that name, but the autoscaler asks for kind %s in %q", t.ref, t.res.APIVersion(), kind, getString(ref, "apiVersion"))
		}
	}
	c.add(l)
}

// checkBinding checks the role and the service accounts of a RoleBinding
// or a ClusterRoleBinding.
func (c *run) checkBinding(o *object) {
	obj := o.res.Object
	roleRef := asMap(obj["roleRef"])
	kind, name := getString(roleRef, "kind"), getString(roleRef, "name")
	switch {
	case name == "":
	case kind == "Role" && o.gk == kindCRB:
		c.add(links.Link{Check: "binding-role", Status: links.StatusBroken, Reason: links.ReasonWrongKind, From: o.ref, Field: "roleRef.kind",
			To:       links.Target{Kind: "Role", Name: name},
			Message:  fmt.Sprintf("%s refers to the Role %q. A ClusterRoleBinding can only refer to a ClusterRole, so Kubernetes rejects it.", o.ref, name),
			Fix:      "Refer to a ClusterRole, or use a RoleBinding in the Role's namespace.",
			Expected: "roleRef.kind to be ClusterRole", Found: "roleRef.kind is Role"})
	case kind == "Role":
		_, l := c.resolve(nameRef{check: "binding-role", from: o, field: "roleRef.name", kind: kindRole, ns: o.ns, name: name,
			why: "a RoleBinding can only refer to a Role in its own namespace"})
		if l.Status == links.StatusUnresolved && l.ClosestMatch == "" && c.byName[kindClusterRole][""][name] != nil {
			l.ClosestMatch = fmt.Sprintf("ClusterRole %s exists; roleRef.kind would have to be ClusterRole to use it", name)
		}
		c.add(l)
	case kind == "ClusterRole":
		_, l := c.resolve(nameRef{check: "binding-role", from: o, field: "roleRef.name", kind: kindClusterRole, name: name})
		if l.Status == links.StatusUnresolved && l.ClosestMatch == "" && o.gk == kindRoleBinding && c.byName[kindRole][o.ns][name] != nil {
			l.ClosestMatch = fmt.Sprintf("Role %s exists in the same namespace; roleRef.kind would have to be Role to use it", name)
		}
		c.add(l)
	}
	for i, s := range getSlice(obj, "subjects") {
		sm := asMap(s)
		if getString(sm, "kind") != "ServiceAccount" || getString(sm, "name") == "" {
			continue // users and groups are not objects in the cluster
		}
		ns := getString(sm, "namespace")
		field := fmt.Sprintf("subjects[%d].name", i)
		if ns == "" {
			if o.gk == kindCRB {
				c.add(links.Link{Check: "binding-subject", Status: links.StatusUnresolved, Reason: links.ReasonInvalid, From: o.ref, Field: field,
					To:      links.Target{Kind: "ServiceAccount", Name: getString(sm, "name")},
					Message: fmt.Sprintf("%s names a ServiceAccount without a namespace, so it could not be followed.", o.ref),
					Fix:     fmt.Sprintf("Set subjects[%d].namespace. A ClusterRoleBinding has no namespace of its own to fall back on.", i)})
				continue
			}
			ns = o.ns
		}
		_, l := c.resolve(nameRef{check: "binding-subject", from: o, field: field, kind: kindServiceAccount, ns: ns, name: getString(sm, "name"),
			why: "the binding names that namespace for the ServiceAccount"})
		c.add(l)
	}
}
