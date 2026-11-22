package checks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// install loads rules from YAML text for one test, and takes them out
// again afterwards.
func install(t *testing.T, yaml string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadRules([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(rules); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Install(nil) })
}

// library installs the rule files shipped in the repository.
func library(t *testing.T) {
	t.Helper()
	rules, err := LoadRules([]string{filepath.Join("..", "..", "rules")})
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(rules); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Install(nil) })
}

// all returns every link of one check.
func all(res *Result, check string) []links.Link {
	var out []links.Link
	for _, l := range res.Links {
		if l.Check == check {
			out = append(out, l)
		}
	}
	return out
}

const widgetRule = `checks:
  - name: widget-gadget
    from: {group: example.com, kind: Widget}
    reference:
      name: spec.gadgetRef.name
      to: {group: example.com, kind: Gadget}
`

func obj(apiVersion, kind, name, ns, rest string) string {
	md := "{name: " + name
	if ns != "" {
		md += ", namespace: " + ns
	}
	return "\n---\napiVersion: " + apiVersion + "\nkind: " + kind + "\nmetadata: " + md + "}\n" + rest
}

func TestRuleFollowsAReferenceByName(t *testing.T) {
	install(t, widgetRule)
	widget := obj("example.com/v1", "Widget", "w", "shop", "spec:\n  gadgetRef: {name: main}\n")
	gadget := func(name, ns string) string { return obj("example.com/v1", "Gadget", name, ns, "") }

	// The rule's name is the check name, like any built-in check.
	res := check(t, widget+gadget("main", "shop"))
	l := one(t, res, "widget-gadget")
	expect(t, l, links.StatusOK, "")
	if l.Field != "spec.gadgetRef.name" || l.To.Kind != "Gadget" || l.To.Name != "main" {
		t.Errorf("link = %+v", l)
	}
	if !strings.Contains(strings.Join(res.Report.ChecksRun, ","), "widget-gadget") {
		t.Errorf("checksRun = %v", res.Report.ChecksRun)
	}

	// A missing target is unresolved, with a hint, exactly as for a
	// built-in check.
	l = one(t, check(t, widget+gadget("mian", "shop")), "widget-gadget")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "Gadget mian") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}
	// A rule cannot make that a pass, and the verdict says so.
	if res := check(t, widget); res.Report.Consistent || res.Report.Complete || res.Report.Status != links.VerdictUnknown {
		t.Errorf("report = %+v", res.Report)
	}
	// The caller's options apply to a rule too.
	expect(t, one(t, check(t, widget, func(r *Request) { r.BundleComplete = true }), "widget-gadget"), links.StatusBroken, links.ReasonNotInBundle)
	expect(t, one(t, check(t, widget, inCluster(t, gadget("main", "shop"))), "widget-gadget"), links.StatusOK, "")

	// The target is looked up in the Widget's own namespace.
	l = one(t, check(t, widget+gadget("main", "other")), "widget-gadget")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "namespace other") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// A kind of the same name in another group is another kind.
	l = one(t, check(t, widget+obj("other.io/v1", "Gadget", "main", "shop", "")), "widget-gadget")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)

	// A rule can be turned off like any other check.
	res = check(t, widget, func(r *Request) { r.SkipChecks = []string{"widget-*"} })
	if len(all(res, "widget-gadget")) != 0 || !strings.Contains(strings.Join(res.Report.ChecksSkipped, ","), "widget-gadget") {
		t.Errorf("skipped rule left links: %+v", res.Report)
	}
}

func TestRuleSaysWhenItFoundNothing(t *testing.T) {
	install(t, widgetRule)
	// A misspelt field leaves a trace, so it is not mistaken for a pass.
	typo := obj("example.com/v1", "Widget", "w", "shop", "spec:\n  gadgetReff: {name: main}\n")
	l := one(t, check(t, typo), "widget-gadget")
	expect(t, l, links.StatusSkipped, links.ReasonNotApplicable)
	if !strings.Contains(l.Message, "has no reference at spec.gadgetRef.name") {
		t.Errorf("message = %q", l.Message)
	}

	// With required, the missing reference is a finding.
	install(t, strings.Replace(widgetRule, "      name: spec.gadgetRef.name\n", "      name: spec.gadgetRef.name\n      required: true\n", 1))
	l = one(t, check(t, typo), "widget-gadget")
	expect(t, l, links.StatusBroken, links.ReasonReferenceMissing)
	if l.Field != "spec.gadgetRef.name" || l.Fix == "" {
		t.Errorf("link = %+v", l)
	}
}

func TestRuleReadsKindGroupAndNamespaceFromTheManifest(t *testing.T) {
	install(t, `checks:
  - name: widget-owner
    from: {group: example.com, kind: Widget}
    reference:
      each: spec.owners[]
      name: name
      namespace: namespace
      to:
        group: example.com
        kind: Gadget
        kindField: kind
        groupField: group
        clusterScoped: [ClusterGadget]
  - name: widget-scale
    from: {group: example.com, kind: Widget}
    reference:
      name: spec.target.name
      to: {group: apps, kind: Deployment, kindField: spec.target.kind, apiVersionField: spec.target.apiVersion}
`)
	widget := obj("example.com/v1", "Widget", "w", "shop", `spec:
  owners:
    - {name: a}
    - {name: b, namespace: other}
    - {name: c, kind: ClusterGadget}
    - {name: d, kind: ConfigMap, group: ""}
    - {name: missing}
  target: {apiVersion: example.com/v1, kind: Gadget, name: a}
`)
	bundle := widget +
		obj("example.com/v1", "Gadget", "a", "shop", "") +
		obj("example.com/v1", "Gadget", "b", "other", "") +
		obj("example.com/v1", "ClusterGadget", "c", "", "") +
		obj("v1", "ConfigMap", "d", "shop", "")
	// With a namespace given, a cluster-scoped kind a rule names is left
	// without one, so it is still found.
	res := check(t, bundle, func(r *Request) { r.Namespace = "elsewhere" })
	got := map[string]links.Link{}
	for _, l := range all(res, "widget-owner") {
		got[l.To.Kind+"/"+l.To.Namespace+"/"+l.To.Name] = l
	}
	for key, want := range map[string]links.Status{
		"Gadget/shop/a": links.StatusOK, "Gadget/other/b": links.StatusOK, "ClusterGadget//c": links.StatusOK,
		"ConfigMap/shop/d": links.StatusOK, "Gadget/shop/missing": links.StatusUnresolved,
	} {
		l, ok := got[key]
		if !ok || l.Status != want {
			t.Errorf("%s: got %q (present %v), want %q; links: %v", key, l.Status, ok, want, keysOfLinks(got))
		}
	}
	if l := got["Gadget/other/b"]; l.Field != "spec.owners[1].name" {
		t.Errorf("field = %q", l.Field)
	}
	// The kind and group come from the reference's own apiVersion and kind.
	l := one(t, res, "widget-scale")
	expect(t, l, links.StatusOK, "")
	if l.To.Kind != "Gadget" {
		t.Errorf("target = %+v", l.To)
	}
	// Without them, the rule's defaults are used.
	bare := obj("example.com/v1", "Widget", "w", "shop", "spec:\n  target: {name: web}\n") + deployment("web", "shop", "{app: web}", "", "")
	expect(t, one(t, check(t, bare), "widget-scale"), links.StatusOK, "")
}

func keysOfLinks(m map[string]links.Link) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRulePortAndKey(t *testing.T) {
	install(t, `checks:
  - name: widget-backend
    from: {group: example.com, kind: Widget}
    reference:
      each: spec.backends[]
      name: name
      port: port
      to: {kind: Service, kindField: kind}
  - name: widget-secret
    from: {group: example.com, kind: Widget}
    reference:
      name: spec.auth.secretName
      key: spec.auth.key
      to: {kind: Secret}
`)
	svc := service("web", "shop", "{app: web}", "[{name: http, port: 80, targetPort: 8080}]") + deployment("web", "shop", "{app: web}", "", "")
	widget := func(backends, auth string) string {
		return obj("example.com/v1", "Widget", "w", "shop", "spec:\n  backends: "+backends+"\n  auth: "+auth+"\n")
	}
	for name, tc := range map[string]struct {
		backend string
		status  links.Status
		reason  links.Reason
	}{
		"port number":     {"{name: web, port: 80}", links.StatusOK, ""},
		"port name":       {"{name: web, port: http}", links.StatusOK, ""},
		"wrong port":      {"{name: web, port: 81}", links.StatusBroken, links.ReasonPortMissing},
		"no port":         {"{name: web}", links.StatusOK, ""},
		"missing service": {"{name: wbe, port: 80}", links.StatusUnresolved, links.ReasonNotInBundle},
		// The port of something that is not a Service is not compared.
		"not a service": {"{name: web, kind: Deployment, port: 80}", links.StatusUnresolved, links.ReasonNotInBundle},
	} {
		t.Run(name, func(t *testing.T) {
			l := one(t, check(t, svc+widget("["+tc.backend+"]", "{}")), "widget-backend")
			expect(t, l, tc.status, tc.reason)
			if name == "wrong port" && (l.Field != "spec.backends[0].port" || !strings.Contains(l.ClosestMatch, "80 (http)")) {
				t.Errorf("link = %+v", l)
			}
		})
	}

	secret := obj("v1", "Secret", "creds", "shop", "stringData: {token: VERY-SECRET-VALUE}\n")
	l := one(t, check(t, secret+widget("[]", "{secretName: creds, key: token}")), "widget-secret")
	expect(t, l, links.StatusOK, "")
	res := check(t, secret+widget("[]", "{secretName: creds, key: tokne}"))
	l = one(t, res, "widget-secret")
	expect(t, l, links.StatusBroken, links.ReasonKeyMissing)
	if l.Field != "spec.auth.key" || !strings.Contains(l.ClosestMatch, "key token") {
		t.Errorf("link = %+v", l)
	}
	// A rule reads key names only, never values.
	for _, x := range res.Links {
		if strings.Contains(x.Message+x.Fix+x.ClosestMatch+x.Found+strings.Join(x.Candidates, ""), "VERY-SECRET-VALUE") {
			t.Errorf("a secret value was copied into a link: %+v", x)
		}
	}
}

func TestRuleSelector(t *testing.T) {
	install(t, `checks:
  - name: monitor-service
    from: {group: example.com, kind: Monitor}
    unlessSet: spec.namespaceSelector
    selector:
      field: spec.selector
      matches: {kind: Service}
  - name: monitor-pods
    from: {group: example.com, kind: PodWatch}
    selector:
      field: spec.podSelector
`)
	svc := "\n---\napiVersion: v1\nkind: Service\nmetadata: {name: web, namespace: shop, labels: {app: web, tier: front}}\nspec: {ports: [{port: 80}]}\n"
	monitor := func(spec string) string { return obj("example.com/v1", "Monitor", "m", "shop", "spec:\n"+spec) }

	l := one(t, check(t, svc+monitor("  selector: {matchLabels: {app: web}}\n")), "monitor-service")
	expect(t, l, links.StatusOK, "")
	if l.To.Kind != "Service" || l.To.Name != "web" {
		t.Errorf("target = %+v", l.To)
	}
	// matchExpressions use the Kubernetes selector library.
	expect(t, one(t, check(t, svc+monitor("  selector:\n    matchExpressions: [{key: tier, operator: In, values: [front, back]}]\n")), "monitor-service"), links.StatusOK, "")
	// A plain label map works as well.
	expect(t, one(t, check(t, svc+monitor("  selector: {app: web}\n")), "monitor-service"), links.StatusOK, "")

	l = one(t, check(t, svc+monitor("  selector: {matchLabels: {app: wbe}}\n")), "monitor-service")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if len(l.Candidates) != 1 || !strings.Contains(l.Candidates[0], "app=web") {
		t.Errorf("candidates = %v", l.Candidates)
	}
	// A Service in another namespace is not selected.
	other := strings.Replace(svc, "namespace: shop", "namespace: other", 1)
	expect(t, one(t, check(t, other+monitor("  selector: {matchLabels: {app: web}}\n")), "monitor-service"), links.StatusUnresolved, links.ReasonNotInBundle)

	// unlessSet: the field changes what the selector means, so the rule
	// steps aside.
	l = one(t, check(t, monitor("  namespaceSelector: {any: true}\n  selector: {matchLabels: {app: nothing}}\n")), "monitor-service")
	expect(t, l, links.StatusSkipped, links.ReasonNotApplicable)
	// No selector, or an empty one, is nothing to follow.
	expect(t, one(t, check(t, monitor("  other: 1\n")), "monitor-service"), links.StatusSkipped, links.ReasonNotApplicable)
	expect(t, one(t, check(t, monitor("  selector: {}\n")), "monitor-service"), links.StatusSkipped, links.ReasonNotApplicable)
	// A selector that is not valid is not followed.
	expect(t, one(t, check(t, monitor("  selector: {app: 5}\n")), "monitor-service"), links.StatusUnresolved, links.ReasonInvalid)

	// Without "matches", the selector is compared with pod templates.
	watch := obj("example.com/v1", "PodWatch", "p", "shop", "spec:\n  podSelector: {matchLabels: {app: web}}\n")
	l = one(t, check(t, deployment("web", "shop", "{app: web}", "", "")+watch), "monitor-pods")
	expect(t, l, links.StatusOK, "")
	if l.To.Kind != "Deployment" {
		t.Errorf("target = %+v", l.To)
	}
	expect(t, one(t, check(t, watch), "monitor-pods"), links.StatusUnresolved, links.ReasonNotInBundle)
}

func TestRuleFilesAreChecked(t *testing.T) {
	write := func(content string) string {
		path := filepath.Join(t.TempDir(), "rules.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Cleanup(func() { Install(nil) })
	ref := func(body string) string {
		return "checks:\n  - name: x\n    from: {kind: Widget}\n    reference:\n" + body
	}
	for name, tc := range map[string]struct{ content, want string }{
		"unknown field":       {"checks:\n  - name: x\n    from: {kind: Widget}\n    refrence: {}\n", "not valid"},
		"no checks":           {"checks: []\n", "has no checks"},
		"bad name":            {"checks:\n  - name: Bad_Name\n    from: {kind: Widget}\n    selector: {field: spec.selector}\n", "the name must be"},
		"no from kind":        {"checks:\n  - name: x\n    selector: {field: spec.selector}\n", "from.kind is required"},
		"neither":             {"checks:\n  - name: x\n    from: {kind: Widget}\n", "exactly one of reference and selector"},
		"both":                {ref("      name: a\n      to: {kind: B}\n    selector: {field: spec.selector}\n"), "exactly one of reference and selector"},
		"no name":             {ref("      to: {kind: B}\n"), "reference.name is required"},
		"no target kind":      {ref("      name: a\n      to: {group: g}\n"), "needs a kind or a kindField"},
		"list in name":        {ref("      name: spec.items[].name\n      to: {kind: B}\n"), "must not contain []"},
		"each without a list": {ref("      each: spec.items\n      name: name\n      to: {kind: B}\n"), "must end in []"},
		"bad path":            {ref("      name: spec..name\n      to: {kind: B}\n"), "is not a valid path"},
		"port and key":        {ref("      name: a\n      port: p\n      key: k\n      to: {kind: B}\n"), "port or key, not both"},
		"two group sources":   {ref("      name: a\n      to: {kind: B, groupField: g, apiVersionField: v}\n"), "not both"},
		"no selector field":   {"checks:\n  - name: x\n    from: {kind: Widget}\n    selector: {}\n", "selector.field is required"},
		"a built-in name":     {"checks:\n  - name: service-selector\n    from: {kind: Widget}\n    selector: {field: spec.selector}\n", "already used by a built-in check"},
		"the same name twice": {"checks:\n  - name: x\n    from: {kind: Widget}\n    selector: {field: a}\n  - name: x\n    from: {kind: Widget}\n    selector: {field: b}\n", "already used by another rule"},
	} {
		t.Run(name, func(t *testing.T) {
			rules, err := LoadRules([]string{write(tc.content)})
			if err == nil {
				err = Install(rules)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	// A rule that is refused leaves the checks as they were.
	Install(nil)
	if err := Install([]Rule{{Name: "service-selector", From: Kind{Kind: "W"}, Selector: &Selector{Field: "a"}}}); err == nil {
		t.Fatal("a built-in name was accepted")
	}
	if len(List()) != len(builtinChecks) {
		t.Errorf("%d checks after a refused rule, want %d", len(List()), len(builtinChecks))
	}

	if _, err := LoadRules([]string{filepath.Join(t.TempDir(), "missing.yaml")}); err == nil {
		t.Error("a missing rules file was accepted")
	}
	if _, err := LoadRules([]string{t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no .yaml or .yml files") {
		t.Errorf("an empty rules directory: %v", err)
	}
	// Nothing to load is fine: rules are optional.
	if rules, err := LoadRules(nil); err != nil || len(rules) != 0 {
		t.Errorf("no rules: %v, %v", rules, err)
	}
}

func TestInstallReplacesTheRules(t *testing.T) {
	install(t, widgetRule)
	if n := len(List()); n != len(builtinChecks)+1 {
		t.Fatalf("%d checks, want %d", n, len(builtinChecks)+1)
	}
	last := List()[len(List())-1]
	if last.Name != "widget-gadget" || last.Source != SourceRule || last.File == "" || last.Description == "" || last.Decides == "" {
		t.Errorf("listed rule = %+v", last)
	}
	for _, c := range List()[:len(builtinChecks)] {
		if c.Source != SourceBuiltin || c.File != "" {
			t.Errorf("built-in check = %+v", c)
		}
	}
	Install(nil)
	if n := len(List()); n != len(builtinChecks) {
		t.Errorf("%d checks after removing the rules, want %d", n, len(builtinChecks))
	}
	// The name is gone with the rule, so naming it is an error again.
	if _, err := Select([]string{"widget-gadget"}, nil); err == nil {
		t.Error("a removed rule can still be selected")
	}
}

// The rule files in the repository load, and each rule follows the
// reference it describes.
func TestRuleLibrary(t *testing.T) {
	library(t)
	gw := "gateway.networking.k8s.io/v1"
	bundle := obj("cert-manager.io/v1", "Certificate", "site", "shop", "spec:\n  secretName: site-tls\n  issuerRef: {name: letsencrypt, kind: ClusterIssuer}\n") +
		obj("cert-manager.io/v1", "ClusterIssuer", "letsencrypt", "", "") +
		obj("cert-manager.io/v1", "Certificate", "internal", "shop", "spec:\n  secretName: internal-tls\n  issuerRef: {name: ca}\n") +
		obj("cert-manager.io/v1", "Certificate", "broken", "shop", "spec:\n  secretName: x\n") +
		obj(gw, "GatewayClass", "public", "", "") +
		obj(gw, "Gateway", "edge", "infra", "spec:\n  gatewayClassName: public\n  listeners:\n    - name: https\n      tls: {certificateRefs: [{name: edge-tls}]}\n") +
		obj("v1", "Secret", "edge-tls", "infra", "") +
		obj(gw, "HTTPRoute", "shop", "shop", "spec:\n  parentRefs: [{name: edge, namespace: infra}, {name: nope}]\n  rules:\n    - backendRefs: [{name: web, port: 80}, {name: web, port: 81}]\n    - backendRefs: [{name: gone, port: 80}]\n") +
		service("web", "shop", "{app: web}", "[{port: 80, targetPort: 8080}]") + deployment("web", "shop", "{app: web}", "", "") +
		obj("external-secrets.io/v1", "ExternalSecret", "db", "shop", "spec:\n  secretStoreRef: {name: vault, kind: ClusterSecretStore}\n") +
		obj("external-secrets.io/v1", "ClusterSecretStore", "vault", "", "") +
		obj("keda.sh/v1alpha1", "ScaledObject", "web", "shop", "spec:\n  scaleTargetRef: {name: web}\n") +
		obj("keda.sh/v1alpha1", "ScaledObject", "db", "shop", "spec:\n  scaleTargetRef: {kind: StatefulSet, name: db}\n") +
		obj("monitoring.coreos.com/v1", "ServiceMonitor", "web", "shop", "spec:\n  selector: {matchLabels: {app: web}}\n") +
		obj("monitoring.coreos.com/v1", "ServiceMonitor", "anywhere", "shop", "spec:\n  namespaceSelector: {any: true}\n  selector: {matchLabels: {app: x}}\n") +
		obj("monitoring.coreos.com/v1", "PodMonitor", "web", "shop", "spec:\n  selector: {matchLabels: {app: web}}\n")
	// The Service in this bundle carries no labels of its own, so give the
	// ServiceMonitor one that does.
	bundle += "\n---\napiVersion: v1\nkind: Service\nmetadata: {name: web-metrics, namespace: shop, labels: {app: web}}\nspec: {ports: [{port: 9100}]}\n"

	res := check(t, bundle, func(r *Request) { r.Namespace = "default" })
	type want struct {
		status links.Status
		reason links.Reason
	}
	for key, w := range map[string]want{
		"certificate-issuer|site|letsencrypt":     {links.StatusOK, ""},
		"certificate-issuer|internal|ca":          {links.StatusUnresolved, links.ReasonNotInBundle},
		"certificate-issuer|broken|":              {links.StatusBroken, links.ReasonReferenceMissing},
		"gateway-class|edge|public":               {links.StatusOK, ""},
		"gateway-certificate|edge|edge-tls":       {links.StatusOK, ""},
		"httproute-parent|shop|edge":              {links.StatusOK, ""},
		"httproute-parent|shop|nope":              {links.StatusUnresolved, links.ReasonNotInBundle},
		"httproute-backend|shop|web:80":           {links.StatusOK, ""},
		"httproute-backend|shop|web:81":           {links.StatusBroken, links.ReasonPortMissing},
		"httproute-backend|shop|gone:80":          {links.StatusUnresolved, links.ReasonNotInBundle},
		"externalsecret-store|db|vault":           {links.StatusOK, ""},
		"scaledobject-target|web|web":             {links.StatusOK, ""},
		"scaledobject-target|db|db":               {links.StatusUnresolved, links.ReasonNotInBundle},
		"servicemonitor-selector|web|web-metrics": {links.StatusOK, ""},
		"servicemonitor-selector|anywhere|":       {links.StatusSkipped, links.ReasonNotApplicable},
		"podmonitor-selector|web|web":             {links.StatusOK, ""},
	} {
		found := false
		for _, l := range res.Links {
			target := l.To.Name
			if l.To.Port != "" {
				target += ":" + l.To.Port
			}
			if l.Check+"|"+l.From.Name+"|"+target != key {
				continue
			}
			found = true
			if l.Status != w.status || l.Reason != w.reason {
				t.Errorf("%s: got %s/%s, want %s/%s (%s)", key, l.Status, l.Reason, w.status, w.reason, l.Message)
			}
		}
		if !found {
			t.Errorf("%s: no such link", key)
		}
	}
	// The ScaledObject that names a StatefulSet is looked up as one.
	for _, l := range all(res, "scaledobject-target") {
		if l.From.Name == "db" && l.To.Kind != "StatefulSet" {
			t.Errorf("target = %+v", l.To)
		}
	}
	// Every rule in the library was exercised above.
	used := map[string]bool{}
	for _, l := range res.Links {
		used[l.Check] = true
	}
	for _, c := range List() {
		if c.Source == SourceRule && !used[c.Name] {
			t.Errorf("the library rule %s is not covered by this test", c.Name)
		}
	}
}
