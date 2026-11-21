package checks

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// Selectors are parsed and matched by the Kubernetes library, so they
// behave here as they do in a cluster. Nothing in this file compares
// labels by hand, except to describe a near miss.

// mapSelector builds a selector from the plain label map a Service uses.
func mapSelector(m map[string]any) (labels.Selector, error) {
	for k, v := range m {
		if _, ok := v.(string); !ok {
			return nil, fmt.Errorf("the value of %q is not a string", k)
		}
	}
	return labels.ValidatedSelectorFromSet(labels.Set(stringMap(m)))
}

// labelSelector builds a selector from a matchLabels and matchExpressions
// object.
func labelSelector(m map[string]any) (labels.Selector, error) {
	var ls metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &ls); err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(&ls)
}

// usesRuntimeLabel names a label in the selector that only running pods
// carry, if there is one.
func usesRuntimeLabel(sel labels.Selector) string {
	reqs, _ := sel.Requirements()
	for _, r := range reqs {
		if runtimeLabels[r.Key()] {
			return r.Key()
		}
	}
	return ""
}

// selectorUse describes one selector to check against the pod templates.
type selectorUse struct {
	check string
	from  *object
	field string
	sel   labels.Selector
	// what the selecting resource does, for messages: "routes to".
	verb string
}

// selectPods records one ok link per pod template the selector matches, or
// one link that says nothing matched. It returns the matches.
func (c *run) selectPods(u selectorUse) []*podTemplate {
	o := u.from
	selText := u.sel.String()
	if key := usesRuntimeLabel(u.sel); key != "" {
		c.add(links.Link{Check: u.check, Status: links.StatusSkipped, Reason: links.ReasonRuntimeLabels, From: o.ref, Field: u.field,
			To:      links.Target{Kind: "Pod", Namespace: o.ns, Selector: selText},
			Message: fmt.Sprintf("%s selects pods by the label %q, which the cluster only adds to running pods. This cannot be checked from manifests.", o.ref, key)})
		return nil
	}
	var matched, sameNS, otherNS []*podTemplate
	for _, t := range c.templates {
		if t.owner == o {
			continue
		}
		if t.owner.ns != o.ns {
			otherNS = append(otherNS, t)
			continue
		}
		sameNS = append(sameNS, t)
		if u.sel.Matches(labels.Set(t.labels)) {
			matched = append(matched, t)
		}
	}
	for _, t := range matched {
		msg := fmt.Sprintf("%s %s the pods of %s (labels %s).", o.ref, u.verb, t.owner.ref, selText)
		if t.owner.cluster {
			msg = fmt.Sprintf("%s %s the pods of %s, which was supplied as existing in the cluster.", o.ref, u.verb, t.owner.ref)
		}
		c.add(links.Link{Check: u.check, Status: links.StatusOK, From: o.ref, Field: u.field,
			To:      links.Target{Kind: t.owner.gk.Kind, Namespace: t.owner.ns, Name: t.owner.name, Selector: selText, Source: t.owner.ref.Source},
			Message: msg})
	}
	if len(matched) > 0 {
		return matched
	}

	where := ""
	if o.ns != "" {
		where = fmt.Sprintf(" in namespace %q", o.ns)
	}
	l := links.Link{Check: u.check, Status: links.StatusUnresolved, Reason: links.ReasonNotInBundle, From: o.ref, Field: u.field,
		To:       links.Target{Kind: "Pod", Namespace: o.ns, Selector: selText},
		Message:  fmt.Sprintf("%s selects pods with labels %s. No pod template in the bundle has them%s.%s", o.ref, selText, where, podsMayExist),
		Fix:      fmt.Sprintf("Make %s agree with the labels of the pods it is meant for. If those pods already exist in the cluster, pass their workload in clusterObjects.", u.field),
		Expected: fmt.Sprintf("a pod template%s with labels %s", where, selText),
		Found:    fmt.Sprintf("%d pod template(s)%s, none with those labels", len(sameNS), where),
	}
	for _, t := range sameNS {
		l.Candidates = append(l.Candidates, fmt.Sprintf("%s has %s", t.owner.ref, labelText(t.labels)))
	}
	l.Candidates = limit(l.Candidates, 20)

	// A full match across an unset namespace: not known to be apart.
	for _, t := range otherNS {
		if u.sel.Matches(labels.Set(t.labels)) && mixedNamespaces(t.owner.ns, o.ns) {
			l.Reason = links.ReasonNamespaceUnknown
			l.Message = fmt.Sprintf("%s selects pods with labels %s. %s has them, but only one of the two sets a namespace, so it is not known whether they end up together.", o.ref, selText, t.owner.ref)
			l.Fix = "Set metadata.namespace on both resources, or pass the namespace the bundle is deployed to."
			c.add(l)
			return nil
		}
	}
	if t, detail := closestTemplate(u.sel, sameNS); t != nil {
		l.ClosestMatch = fmt.Sprintf("%s: %s", t.owner.ref, detail)
	} else {
		for _, t := range otherNS {
			if u.sel.Matches(labels.Set(t.labels)) {
				l.ClosestMatch = fmt.Sprintf("%s has these labels, but it is in another namespace and a selector only reaches pods in its own", t.owner.ref)
				break
			}
		}
	}
	c.add(l)
	return nil
}

// closestTemplate picks the pod template that comes nearest to matching
// and says what differs. It is only used to word a hint.
func closestTemplate(sel labels.Selector, templates []*podTemplate) (*podTemplate, string) {
	reqs, _ := sel.Requirements()
	var best *podTemplate
	bestScore, bestDetail := 0, ""
	for _, t := range templates {
		score := 0
		var diffs []string
		for _, r := range reqs {
			if r.Matches(labels.Set(t.labels)) {
				score += 3
				continue
			}
			want := wanted(r)
			have, has := t.labels[r.Key()]
			switch {
			case has:
				score++
				if want != "" && distance(want, have) <= 2 {
					score++
				}
				diffs = append(diffs, fmt.Sprintf("it has %s=%s where the selector wants %s", r.Key(), have, r.String()))
			case want != "" && hasValue(t.labels, want):
				score++
				diffs = append(diffs, fmt.Sprintf("it has the value %s under another label (%s) where the selector wants %s", want, labelText(t.labels), r.String()))
			default:
				if k, ok := nearest(r.Key(), sortedKeys(t.labels)); ok {
					score++
					diffs = append(diffs, fmt.Sprintf("it has the label %s=%s where the selector wants %s", k, t.labels[k], r.String()))
				} else {
					diffs = append(diffs, fmt.Sprintf("it has no label %s", r.Key()))
				}
			}
		}
		if score > bestScore || (score == bestScore && best != nil && score > 0 && t.owner.name < best.owner.name) {
			best, bestScore, bestDetail = t, score, strings.Join(diffs, "; ")
		}
	}
	return best, bestDetail
}

// wanted returns the single value a requirement asks for, if it has one.
func wanted(r labels.Requirement) string {
	switch r.Operator() {
	case selection.Equals, selection.DoubleEquals, selection.In:
		if v := r.Values().List(); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func hasValue(l map[string]string, v string) bool {
	for _, have := range l {
		if have == v {
			return true
		}
	}
	return false
}

// checkService checks a Service's selector, then each of its target ports
// against the pods it selects.
func (c *run) checkService(o *object) {
	spec := o.spec()
	raw, _ := spec["selector"].(map[string]any)
	if len(raw) == 0 || getString(spec, "type") == "ExternalName" {
		c.add(links.Link{Check: "service-selector", Status: links.StatusSkipped, Reason: links.ReasonNotApplicable, From: o.ref, Field: "spec.selector",
			To:      links.Target{Kind: "Pod", Namespace: o.ns},
			Message: fmt.Sprintf("%s has no selector, so it does not pick pods by label.", o.ref)})
		return
	}
	sel, err := mapSelector(raw)
	if err != nil {
		c.add(invalidSelector(o, "service-selector", "spec.selector", err))
		return
	}
	matched := c.selectPods(selectorUse{check: "service-selector", from: o, field: "spec.selector", sel: sel, verb: "routes to"})
	for i, p := range getSlice(spec, "ports") {
		pm := asMap(p)
		field := fmt.Sprintf("spec.ports[%d].targetPort", i)
		target, isNum, ok := portValue(pm["targetPort"])
		if !ok {
			// With no targetPort, the Service sends to the same number.
			if target, isNum, ok = portValue(pm["port"]); !ok {
				continue
			}
			field = fmt.Sprintf("spec.ports[%d].port", i)
		}
		// A named port that some of the selected pods declare still works
		// for those pods, so its absence from the others is not certain to
		// be a mistake.
		someHave := false
		for _, t := range matched {
			if byName, _ := t.ports(); !isNum && byName[target] != "" {
				someHave = true
			}
		}
		for _, t := range matched {
			c.add(servicePortLink(o, t, field, target, isNum, someHave))
		}
	}
}

func invalidSelector(o *object, check, field string, err error) links.Link {
	return links.Link{Check: check, Status: links.StatusSkipped, Reason: links.ReasonInvalid, From: o.ref, Field: field,
		To:      links.Target{Kind: "Pod", Namespace: o.ns},
		Message: fmt.Sprintf("The selector of %s is not valid, so it was not followed: %v. A schema check reports this in full.", o.ref, err)}
}

// servicePortLink checks one Service target port against one selected pod
// template. othersHaveIt says that another selected template declares the
// named port.
func servicePortLink(svc *object, t *podTemplate, field, port string, isNum, othersHaveIt bool) links.Link {
	byName, numbers := t.ports()
	l := links.Link{Check: "service-port", From: svc.ref, Field: field,
		To: links.Target{Kind: t.owner.gk.Kind, Namespace: t.owner.ns, Name: t.owner.name, Port: port, Source: t.owner.ref.Source}}
	var declared []string
	for _, n := range sortedKeys(numbers) {
		declared = append(declared, n)
	}
	sort.Slice(declared, func(i, j int) bool {
		return len(declared[i]) < len(declared[j]) || (len(declared[i]) == len(declared[j]) && declared[i] < declared[j])
	})
	for i, n := range declared {
		for name, num := range byName {
			if num == n {
				declared[i] = fmt.Sprintf("%s (%s)", n, name)
			}
		}
	}
	l.Candidates = declared
	l.Found = "container ports: " + joinList(declared)
	if len(declared) == 0 {
		l.Found = "no container ports declared"
	}

	if !isNum {
		l.Expected = fmt.Sprintf("a container port named %q on the pods of %s", port, t.owner.ref)
		if _, ok := byName[port]; ok {
			l.Status = links.StatusOK
			l.Message = fmt.Sprintf("%s sends to the port named %q, which the pods of %s declare.", svc.ref, port, t.owner.ref)
			return l
		}
		l.Status, l.Reason = links.StatusBroken, links.ReasonPortMissing
		l.Message = fmt.Sprintf("%s sends to the port named %q, and the pods of %s declare no port with that name. A named port only works if a container declares it.", svc.ref, port, t.owner.ref)
		l.Fix = fmt.Sprintf("Name a container port %q in %s, or change %s to a port the pods declare.", port, t.owner.ref, field)
		if othersHaveIt {
			l.Status, l.Reason = links.StatusUnresolved, links.ReasonPortNotDeclared
			l.Message = fmt.Sprintf("%s sends to the port named %q. The pods of %s declare no port with that name, so they get no traffic on it. Other pods the Service selects do declare it, so this may be intended.", svc.ref, port, t.owner.ref)
		}
		if n, ok := nearest(port, sortedKeys(byName)); ok {
			l.ClosestMatch = fmt.Sprintf("port %s (%s) of %s", n, byName[n], t.owner.ref)
		} else if len(declared) > 0 {
			l.ClosestMatch = fmt.Sprintf("%s declares %s", t.owner.ref, joinList(declared))
		}
		return l
	}

	l.Expected = fmt.Sprintf("the pods of %s to listen on port %s", t.owner.ref, port)
	switch {
	case numbers[port]:
		l.Status = links.StatusOK
		l.Message = fmt.Sprintf("%s sends to port %s, which the pods of %s declare.", svc.ref, port, t.owner.ref)
	case len(numbers) == 0:
		// Declaring ports is optional, and these pods declare none.
		l.Status, l.Reason = links.StatusSkipped, links.ReasonPortNotDeclared
		l.Message = fmt.Sprintf("%s sends to port %s. The pods of %s declare no ports at all, which Kubernetes allows, so there is nothing to compare.", svc.ref, port, t.owner.ref)
	default:
		l.Status, l.Reason = links.StatusUnresolved, links.ReasonPortNotDeclared
		l.Message = fmt.Sprintf("%s sends to port %s, and the pods of %s declare %s but not %s. Kubernetes does not require a port to be declared, so this may still work.", svc.ref, port, t.owner.ref, joinList(declared), port)
		l.Fix = fmt.Sprintf("Check which port the container listens on. Then change %s, or add the port to the container's ports.", field)
		l.ClosestMatch = fmt.Sprintf("%s declares %s", t.owner.ref, joinList(declared))
	}
	return l
}

// checkPodSelector checks the pod selector of a PodDisruptionBudget or a
// NetworkPolicy.
func (c *run) checkPodSelector(o *object, check, field string) {
	raw, present := o.spec()[field].(map[string]any)
	if !present || (len(getMap(raw, "matchLabels")) == 0 && len(getSlice(raw, "matchExpressions")) == 0) {
		c.add(links.Link{Check: check, Status: links.StatusSkipped, Reason: links.ReasonNotApplicable, From: o.ref, Field: "spec." + field,
			To:      links.Target{Kind: "Pod", Namespace: o.ns},
			Message: fmt.Sprintf("%s has an empty pod selector, so it does not pick pods by label.", o.ref)})
		return
	}
	sel, err := labelSelector(raw)
	if err != nil {
		c.add(invalidSelector(o, check, "spec."+field, err))
		return
	}
	c.selectPods(selectorUse{check: check, from: o, field: "spec." + field, sel: sel, verb: "applies to"})
}

// checkWorkloadSelector checks that a workload's selector matches its own
// pod template. Both ends are in the same resource, so this is certain.
func (c *run) checkWorkloadSelector(o *object) {
	spec := o.spec()
	var sel labels.Selector
	var err error
	rejected := true
	switch o.gk {
	case kindDeployment, kindReplicaSet, kindStatefulSet, kindDaemonSet:
		raw, ok := spec["selector"].(map[string]any)
		if !ok {
			return
		}
		sel, err = labelSelector(raw)
	case kindJob:
		// A Job writes its own selector unless told otherwise.
		raw, ok := spec["selector"].(map[string]any)
		if !ok || !getBool(spec, "manualSelector") {
			return
		}
		sel, err = labelSelector(raw)
	case kindRC, kindDC:
		raw, ok := spec["selector"].(map[string]any)
		if !ok || len(raw) == 0 {
			return
		}
		sel, err = mapSelector(raw)
		rejected = o.gk == kindRC
	default:
		return
	}
	if err != nil {
		c.add(invalidSelector(o, "workload-selector", "spec.selector", err))
		return
	}
	ts := templatesOf(o)
	if len(ts) == 0 {
		return
	}
	t := ts[0]
	l := links.Link{Check: "workload-selector", From: o.ref, Field: "spec.selector",
		To: links.Target{Kind: o.gk.Kind, Namespace: o.ns, Name: o.name, Selector: sel.String(), Source: o.ref.Source}}
	if sel.Matches(labels.Set(t.labels)) {
		l.Status = links.StatusOK
		l.Message = fmt.Sprintf("The selector of %s matches its own pod template.", o.ref)
		c.add(l)
		return
	}
	l.Status, l.Reason = links.StatusBroken, links.ReasonSelectorMismatch
	l.Expected = "pod template labels that satisfy " + sel.String()
	l.Found = "pod template labels: " + labelText(t.labels)
	l.Candidates = []string{"spec.template.metadata.labels: " + labelText(t.labels)}
	l.Message = fmt.Sprintf("The selector of %s (%s) does not match its own pod template labels (%s).", o.ref, sel.String(), labelText(t.labels))
	if rejected {
		l.Message += " Kubernetes rejects the resource."
	} else {
		l.Message += " The workload would not manage the pods it creates."
	}
	l.Fix = "Make spec.selector and spec.template.metadata.labels agree."
	if _, detail := closestTemplate(sel, ts); detail != "" {
		l.ClosestMatch = "its own pod template: " + detail
	}
	c.add(l)
}
