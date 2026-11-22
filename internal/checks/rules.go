package checks

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
	"github.com/cosmosdevops/kube-refs-mcp/internal/manifests"
)

// A rule is a check that an operator adds from a file. It says where one
// kind of resource refers to another: which field holds the name, or the
// selector, and what kind the target is.
//
// A rule cannot decide a result. It only points at a reference. The status
// is worked out by the same code as for the built-in checks, so a rule can
// never report a pass that was not established.

// Kind names a kind of resource by API group and kind.
type Kind struct {
	// Group is the API group. Empty means the core group.
	Group string `json:"group,omitempty"`
	Kind  string `json:"kind"`
}

func (k Kind) gk() gk { return gk{k.Group, k.Kind} }

func (k Kind) String() string {
	if k.Group == "" {
		return k.Kind
	}
	return k.Kind + "." + k.Group
}

// Target says what a reference points at.
type Target struct {
	// Group and Kind are used when the manifest does not say otherwise.
	Group string `json:"group,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// KindField, GroupField and APIVersionField are paths to fields that
	// name the target's kind and group, as a ref object often has. A field
	// that is set in the manifest wins over Group and Kind.
	KindField       string `json:"kindField,omitempty"`
	GroupField      string `json:"groupField,omitempty"`
	APIVersionField string `json:"apiVersionField,omitempty"`
	// ClusterScoped lists target kinds that have no namespace.
	ClusterScoped []string `json:"clusterScoped,omitempty"`
}

// Reference describes a reference by name.
type Reference struct {
	// Each is a path to a list. The reference is then read once from every
	// item. It ends in [], and may pass through other lists:
	// "spec.rules[].backendRefs[]". Without it the reference is read once
	// from the resource.
	Each string `json:"each,omitempty"`
	// Name is the path to the field that holds the target's name. With
	// Each it is relative to the item.
	Name string `json:"name"`
	// Namespace is the path to a field that names the target's namespace.
	// Without it, or when the field is not set, the target is looked up in
	// the namespace of the resource that refers to it.
	Namespace string `json:"namespace,omitempty"`
	To        Target `json:"to"`
	// Port is the path to a field that holds a port of the target, which
	// must then be a Service.
	Port string `json:"port,omitempty"`
	// Key is the path to a field that holds a key of the target, which
	// must then be a ConfigMap or a Secret.
	Key string `json:"key,omitempty"`
	// Required says the reference must be there. A resource without it is
	// then reported, which helps for kinds that have no schema to check.
	Required bool `json:"required,omitempty"`
}

// Selector describes a reference by labels.
type Selector struct {
	// Field is the path to the selector. It may be a plain label map, or an
	// object with matchLabels and matchExpressions.
	Field string `json:"field"`
	// Matches is the kind whose labels are compared. Without it the
	// selector is compared with pod templates, as a Service's is.
	Matches *Kind `json:"matches,omitempty"`
}

// Rule is one check added from a file.
type Rule struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// From is the kind of resource the reference is read from.
	From Kind `json:"from"`
	// UnlessSet is a path. When the resource sets that field, the rule
	// does not apply to it. It is for a field that changes what the
	// reference means, such as a selector that reaches other namespaces.
	UnlessSet string     `json:"unlessSet,omitempty"`
	Reference *Reference `json:"reference,omitempty"`
	Selector  *Selector  `json:"selector,omitempty"`

	// file is where the rule was read from, for messages.
	file string
}

type ruleFile struct {
	Checks []Rule `json:"checks"`
}

const (
	maxRuleFileBytes = 1 << 20
	maxRules         = 500
)

var (
	ruleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	// A path is field names joined by dots. A name may end in [] to read
	// every item of a list.
	pathPart = regexp.MustCompile(`^[A-Za-z0-9_-]+(\[\])?$`)
)

// LoadRules reads rules from files and directories. A directory is read
// for .yaml and .yml files, in name order. Anything wrong with a file is an
// error: a rule is never skipped quietly.
func LoadRules(paths []string) ([]Rule, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("reading rules: %w", err)
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		var found []string
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			ext := strings.ToLower(filepath.Ext(path))
			if d.Type().IsRegular() && (ext == ".yaml" || ext == ".yml") {
				found = append(found, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading rules: %w", err)
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("the rules directory %s has no .yaml or .yml files", p)
		}
		sort.Strings(found)
		files = append(files, found...)
	}

	var rules []Rule
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("reading rules: %w", err)
		}
		if info.Size() > maxRuleFileBytes {
			return nil, fmt.Errorf("the rules file %s is larger than %d bytes", f, maxRuleFileBytes)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading rules: %w", err)
		}
		var rf ruleFile
		if err := yaml.UnmarshalStrict(data, &rf); err != nil {
			return nil, fmt.Errorf("the rules file %s is not valid: %w", f, err)
		}
		if len(rf.Checks) == 0 {
			return nil, fmt.Errorf("the rules file %s has no checks", f)
		}
		for i := range rf.Checks {
			rf.Checks[i].file = f
		}
		rules = append(rules, rf.Checks...)
	}
	if len(rules) > maxRules {
		return nil, fmt.Errorf("%d rules were given, which is over the limit of %d", len(rules), maxRules)
	}
	return rules, nil
}

// validate checks one rule and fills in its description.
func (r *Rule) validate() error {
	bad := func(format string, args ...any) error {
		where := fmt.Sprintf("rule %q", r.Name)
		if r.file != "" {
			where += " in " + r.file
		}
		return fmt.Errorf("%s: %s", where, fmt.Sprintf(format, args...))
	}
	if !ruleName.MatchString(r.Name) {
		return bad("the name must be lower-case letters, digits and dashes, starting with a letter")
	}
	if r.From.Kind == "" {
		return bad("from.kind is required")
	}
	if (r.Reference == nil) == (r.Selector == nil) {
		return bad("give exactly one of reference and selector")
	}
	path := func(name, value string, list bool) error {
		if value == "" {
			return nil
		}
		parts := strings.Split(value, ".")
		for i, p := range parts {
			if !pathPart.MatchString(p) {
				return bad("%s: %q is not a valid path; use field names joined by dots", name, value)
			}
			if strings.HasSuffix(p, "[]") && !list {
				return bad("%s: %q must not contain []; only each may read a list", name, value)
			}
			if list && i == len(parts)-1 && !strings.HasSuffix(p, "[]") {
				return bad("%s: %q must end in [], because it names a list", name, value)
			}
		}
		return nil
	}
	if err := path("unlessSet", r.UnlessSet, false); err != nil {
		return err
	}
	if ref := r.Reference; ref != nil {
		if ref.Name == "" {
			return bad("reference.name is required")
		}
		if ref.To.Kind == "" && ref.To.KindField == "" {
			return bad("reference.to needs a kind or a kindField")
		}
		if ref.To.GroupField != "" && ref.To.APIVersionField != "" {
			return bad("reference.to: give groupField or apiVersionField, not both")
		}
		if ref.Port != "" && ref.Key != "" {
			return bad("reference: give port or key, not both")
		}
		for _, f := range []struct {
			name, value string
			list        bool
		}{
			{"reference.each", ref.Each, true}, {"reference.name", ref.Name, false}, {"reference.namespace", ref.Namespace, false},
			{"reference.to.kindField", ref.To.KindField, false}, {"reference.to.groupField", ref.To.GroupField, false},
			{"reference.to.apiVersionField", ref.To.APIVersionField, false}, {"reference.port", ref.Port, false}, {"reference.key", ref.Key, false},
		} {
			if err := path(f.name, f.value, f.list); err != nil {
				return err
			}
		}
		if r.Description == "" {
			what := ref.To.Kind
			if what == "" {
				what = "resource"
			}
			r.Description = fmt.Sprintf("The %s that %s %s names exists.", what, article(r.From.Kind), r.From.Kind)
		}
	}
	if sel := r.Selector; sel != nil {
		if sel.Field == "" {
			return bad("selector.field is required")
		}
		if err := path("selector.field", sel.Field, false); err != nil {
			return err
		}
		if sel.Matches != nil && sel.Matches.Kind == "" {
			return bad("selector.matches.kind is required when matches is given")
		}
		if r.Description == "" {
			what := "the pod template of a workload"
			if sel.Matches != nil {
				what = fmt.Sprintf("%s %s", article(sel.Matches.Kind), sel.Matches.Kind)
			}
			r.Description = fmt.Sprintf("The selector of %s %s matches %s.", article(r.From.Kind), r.From.Kind, what)
		}
	}
	return nil
}

func (r *Rule) decides() string {
	switch {
	case r.Selector != nil:
		return "Unresolved when it matches nothing."
	case r.Reference.Port != "":
		return "Broken when the Service is in the bundle without the port. Unresolved when the target is not in the bundle."
	case r.Reference.Key != "":
		return "Broken when the target is in the bundle without the key. Unresolved when the target is not in the bundle."
	}
	return "Unresolved when the target is not in the bundle."
}

// builtin is how many checks and runners the program has before any rule
// is installed.
var builtin = struct{ checks, runners int }{len(registry), len(runners)}

// Install adds the rules to the checks. It replaces any rules installed
// before, so it can be called again with a new set. It is meant for
// start-up: it must not run while a check is in progress.
func Install(rules []Rule) error {
	names := map[string]string{}
	for _, c := range registry[:builtin.checks] {
		names[c.Name] = "a built-in check"
	}
	var scoped []string
	for i := range rules {
		r := &rules[i]
		if err := r.validate(); err != nil {
			return err
		}
		if what, taken := names[r.Name]; taken {
			return fmt.Errorf("rule %q in %s: the name is already used by %s", r.Name, r.file, what)
		}
		names[r.Name] = "another rule, in " + r.file
		if r.Reference != nil {
			scoped = append(scoped, r.Reference.To.ClusterScoped...)
		}
	}

	registry = registry[:builtin.checks:builtin.checks]
	runners = runners[:builtin.runners:builtin.runners]
	for i := range rules {
		r := rules[i]
		registry = append(registry, Check{Name: r.Name, Description: r.Description, Kinds: []string{r.From.Kind}, Decides: r.decides(), Source: SourceRule, File: r.file})
		runners = append(runners, runner{names: []string{r.Name}, kinds: []gk{r.From.gk()}, fn: r.run})
	}
	registered = map[string]bool{}
	for _, c := range registry {
		registered[c.Name] = true
	}
	manifests.SetExtraClusterScoped(scoped)
	return nil
}

// run follows the rule's reference from one resource.
func (r Rule) run(c *run, o *object) {
	obj := o.res.Object
	if r.UnlessSet != "" {
		if v, ok := lookup(obj, r.UnlessSet); ok && v != nil {
			c.add(links.Link{Check: r.Name, Status: links.StatusSkipped, Reason: links.ReasonNotApplicable, From: o.ref, Field: r.UnlessSet,
				Message: fmt.Sprintf("%s sets %s, so the rule %s does not apply to it.", o.ref, r.UnlessSet, r.Name)})
			return
		}
	}
	if r.Selector != nil {
		r.runSelector(c, o)
		return
	}
	r.runReference(c, o)
}

func (r Rule) runReference(c *run, o *object) {
	ref := r.Reference
	items := []item{{value: o.res.Object}}
	if ref.Each != "" {
		items = each(o.res.Object, ref.Each)
	}
	join := func(base, rel string) string {
		if base == "" {
			return rel
		}
		return base + "." + rel
	}
	found := 0
	for _, it := range items {
		m := asMap(it.value)
		field := join(it.path, ref.Name)
		name := lookupString(m, ref.Name)
		if name == "" {
			if ref.Required {
				c.add(links.Link{Check: r.Name, Status: links.StatusBroken, Reason: links.ReasonReferenceMissing, From: o.ref, Field: field,
					To:       links.Target{Kind: ref.To.Kind},
					Message:  fmt.Sprintf("%s does not set %s. The rule %s says the reference must be there.", o.ref, field, r.Name),
					Fix:      fmt.Sprintf("Set %s.", field),
					Expected: field + " to be set", Found: "the field is not set"})
			}
			continue
		}
		found++

		target := gk{ref.To.Group, ref.To.Kind}
		if v := lookupString(m, ref.To.KindField); v != "" {
			target.Kind = v
		}
		// A group field that is present and empty names the core group.
		if ref.To.GroupField != "" {
			if v, ok := lookup(m, ref.To.GroupField); ok {
				s, _ := v.(string)
				target.Group = s
			}
		}
		if v := lookupString(m, ref.To.APIVersionField); v != "" {
			target.Group = groupOf(v)
		}

		ns := o.ns
		why := fmt.Sprintf("the rule %s looks in the namespace of the %s", r.Name, o.gk.Kind)
		if v := lookupString(m, ref.Namespace); v != "" {
			ns = v
			why = fmt.Sprintf("%s names that namespace", join(it.path, ref.Namespace))
		}
		if manifests.IsClusterScoped(target.Kind) {
			ns = ""
		}
		t, l := c.resolve(nameRef{check: r.Name, from: o, field: field, kind: target, ns: ns, name: name, why: why})

		switch {
		case t == nil:
			if p, _, ok := portValue(lookupAny(m, ref.Port)); ok {
				l.To.Port = p
			}
			if k := lookupString(m, ref.Key); k != "" {
				l.To.Key = k
			}
		case ref.Port != "":
			p, _, ok := portValue(lookupAny(m, ref.Port))
			if !ok {
				break
			}
			if t.gk != kindService {
				l.To.Port = p
				l.Status, l.Reason = links.StatusSkipped, links.ReasonNotApplicable
				l.Message = fmt.Sprintf("%s refers to port %s of %s, which is not a Service, so the port was not compared.", o.ref, p, t.ref)
				break
			}
			l = c.portLink(o, t, l, p, join(it.path, ref.Port))
		case ref.Key != "":
			k := lookupString(m, ref.Key)
			if k == "" || (t.gk != kindConfigMap && t.gk != kindSecret) {
				break
			}
			l = c.keyLink(o, t, join(it.path, ref.Key), k, false)
			l.Check = r.Name
		}
		c.add(l)
	}
	// Say that the rule ran and found nothing to follow. Without this, a
	// misspelt field in a kind that has no schema would leave no trace.
	if found == 0 && !ref.Required {
		at := join(strings.TrimSuffix(ref.Each, "[]"), ref.Name)
		if ref.Each != "" {
			at = ref.Each + "." + ref.Name
		}
		c.add(links.Link{Check: r.Name, Status: links.StatusSkipped, Reason: links.ReasonNotApplicable, From: o.ref, Field: at,
			To:      links.Target{Kind: ref.To.Kind},
			Message: fmt.Sprintf("%s has no reference at %s, so the rule %s had nothing to follow.", o.ref, at, r.Name)})
	}
}

func (r Rule) runSelector(c *run, o *object) {
	sel := r.Selector
	raw, _ := lookupAny(o.res.Object, sel.Field).(map[string]any)
	what := "Pod"
	if sel.Matches != nil {
		what = sel.Matches.Kind
	}
	_, hasLabels := raw["matchLabels"]
	_, hasExprs := raw["matchExpressions"]
	structured := hasLabels || hasExprs
	empty := len(raw) == 0 || (structured && len(getMap(raw, "matchLabels")) == 0 && len(getSlice(raw, "matchExpressions")) == 0)
	if empty {
		c.add(links.Link{Check: r.Name, Status: links.StatusSkipped, Reason: links.ReasonNotApplicable, From: o.ref, Field: sel.Field,
			To:      links.Target{Kind: what, Namespace: o.ns},
			Message: fmt.Sprintf("%s has no selector at %s, or an empty one, so the rule %s had nothing to follow.", o.ref, sel.Field, r.Name)})
		return
	}
	var s labels.Selector
	var err error
	if structured {
		s, err = labelSelector(raw)
	} else {
		s, err = mapSelector(raw)
	}
	if err != nil {
		c.add(invalidSelector(o, r.Name, sel.Field, err))
		return
	}
	if sel.Matches == nil {
		c.selectPods(selectorUse{check: r.Name, from: o, field: sel.Field, sel: s, verb: "selects"})
		return
	}
	c.selectObjects(r.Name, o, sel.Field, s, sel.Matches.gk())
}

// selectObjects compares a selector with the labels of every object of one
// kind in the same namespace. It records one ok link per match, or one link
// that says nothing matched.
func (c *run) selectObjects(check string, o *object, field string, sel labels.Selector, kind gk) {
	selText := sel.String()
	clusterScoped := manifests.IsClusterScoped(kind.Kind)
	var names []string
	for name := range c.byName[kind][nsFor(o, clusterScoped)] {
		names = append(names, name)
	}
	sort.Strings(names)
	matched := 0
	var candidates []string
	for _, name := range names {
		t := c.byName[kind][nsFor(o, clusterScoped)][name]
		have := stringMap(getMap(t.res.Object, "metadata", "labels"))
		candidates = append(candidates, fmt.Sprintf("%s has %s", t.ref, labelText(have)))
		if !sel.Matches(labels.Set(have)) {
			continue
		}
		matched++
		msg := fmt.Sprintf("%s selects %s (labels %s).", o.ref, t.ref, selText)
		if t.cluster {
			msg = fmt.Sprintf("%s selects %s, which was supplied as existing in the cluster.", o.ref, t.ref)
		}
		c.add(links.Link{Check: check, Status: links.StatusOK, From: o.ref, Field: field,
			To:      links.Target{Kind: kind.Kind, Namespace: t.ns, Name: t.name, Selector: selText, Source: t.ref.Source},
			Message: msg})
	}
	if matched > 0 {
		return
	}
	where := ""
	if !clusterScoped && o.ns != "" {
		where = fmt.Sprintf(" in namespace %q", o.ns)
	}
	l := links.Link{Check: check, Status: links.StatusUnresolved, Reason: links.ReasonNotInBundle, From: o.ref, Field: field,
		To:         links.Target{Kind: kind.Kind, Namespace: nsFor(o, clusterScoped), Selector: selText},
		Message:    fmt.Sprintf("%s selects %s resources with labels %s. None in the bundle has them%s.%s", o.ref, kind.Kind, selText, where, mayExist),
		Fix:        fmt.Sprintf("Make %s agree with the labels of the %s it is meant for. If that %s already exists in the cluster, pass it in clusterObjects.", field, kind.Kind, kind.Kind),
		Expected:   fmt.Sprintf("%s %s%s with labels %s", article(kind.Kind), kind.Kind, where, selText),
		Found:      fmt.Sprintf("%d %s resource(s)%s, none with those labels", len(names), kind.Kind, where),
		Candidates: limit(candidates, 20),
	}
	// A match on the other side of an unset namespace: not known to be apart.
	if !clusterScoped {
		for ns, byName := range c.byName[kind] {
			if !mixedNamespaces(ns, o.ns) {
				continue
			}
			for _, t := range byName {
				if sel.Matches(labels.Set(stringMap(getMap(t.res.Object, "metadata", "labels")))) {
					l.Reason = links.ReasonNamespaceUnknown
					l.Message = fmt.Sprintf("%s selects %s resources with labels %s. %s has them, but only one of the two sets a namespace, so it is not known whether they end up together.", o.ref, kind.Kind, selText, t.ref)
					l.Fix = "Set metadata.namespace on both resources, or pass the namespace the bundle is deployed to."
				}
			}
		}
	}
	c.add(l)
}

func nsFor(o *object, clusterScoped bool) string {
	if clusterScoped {
		return ""
	}
	return o.ns
}

// item is one value found by a path, with the path that led to it.
type item struct {
	path  string
	value any
}

// each reads every item of the list a path names. The path may pass
// through other lists: "spec.rules[].backendRefs[]".
func each(obj map[string]any, path string) []item {
	cur := []item{{value: obj}}
	for _, part := range strings.Split(path, ".") {
		name, list := strings.CutSuffix(part, "[]")
		var next []item
		for _, it := range cur {
			m := asMap(it.value)
			if m == nil {
				continue
			}
			v, ok := m[name]
			if !ok {
				continue
			}
			p := name
			if it.path != "" {
				p = it.path + "." + name
			}
			if !list {
				next = append(next, item{p, v})
				continue
			}
			s, _ := v.([]any)
			for i, x := range s {
				next = append(next, item{fmt.Sprintf("%s[%d]", p, i), x})
			}
		}
		cur = next
	}
	return cur
}

// lookup follows a dotted path of field names. It reports whether the
// field is there at all.
func lookup(m map[string]any, path string) (any, bool) {
	if path == "" || m == nil {
		return nil, false
	}
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	v, ok := m[parts[len(parts)-1]]
	return v, ok
}

func lookupAny(m map[string]any, path string) any {
	v, _ := lookup(m, path)
	return v
}

func lookupString(m map[string]any, path string) string {
	s, _ := lookupAny(m, path).(string)
	return s
}
