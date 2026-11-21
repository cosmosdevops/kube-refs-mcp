package checks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
	"github.com/cosmosdevops/kube-refs-mcp/internal/manifests"
)

func parse(t *testing.T, yaml, file string) []manifests.Resource {
	t.Helper()
	rs, err := manifests.Parse([]byte(yaml), file, manifests.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// check runs the checks on a bundle. opts may change the request.
func check(t *testing.T, yaml string, opts ...func(*Request)) *Result {
	t.Helper()
	req := Request{Resources: parse(t, yaml, "inline")}
	for _, o := range opts {
		o(&req)
	}
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func complete(r *Request) { r.BundleComplete = true }

func inCluster(t *testing.T, yaml string) func(*Request) {
	return func(r *Request) { r.ClusterObjects = parse(t, yaml, "cluster") }
}

// one returns the only link of a check, and fails if there is not exactly
// one.
func one(t *testing.T, res *Result, check string) links.Link {
	t.Helper()
	var hits []links.Link
	for _, l := range res.Links {
		if l.Check == check {
			hits = append(hits, l)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want one %s link, got %d: %+v", check, len(hits), res.Links)
	}
	return hits[0]
}

func expect(t *testing.T, l links.Link, status links.Status, reason links.Reason) {
	t.Helper()
	if l.Status != status || l.Reason != reason {
		t.Errorf("%s: got %s/%s, want %s/%s (%s)", l.Check, l.Status, l.Reason, status, reason, l.Message)
	}
}

// deployment makes a Deployment with one container.
func deployment(name, ns, labels, containerExtra, podExtra string) string {
	nsLine := ""
	if ns != "" {
		nsLine = "\n  namespace: " + ns
	}
	return `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + nsLine + `
spec:
  selector: {matchLabels: ` + labels + `}
  template:
    metadata: {labels: ` + labels + `}
    spec:
` + indent(podExtra, 6) + `      containers:
        - name: c
          image: example.com/app:1
` + indent(containerExtra, 10)
}

func indent(s string, n int) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	pad := strings.Repeat(" ", n)
	var out []string
	for _, line := range strings.Split(strings.Trim(s, "\n"), "\n") {
		out = append(out, pad+line)
	}
	return strings.Join(out, "\n") + "\n"
}

func service(name, ns, selector, ports string) string {
	nsLine := ""
	if ns != "" {
		nsLine = "\n  namespace: " + ns
	}
	return `
---
apiVersion: v1
kind: Service
metadata:
  name: ` + name + nsLine + `
spec:
  selector: ` + selector + `
  ports: ` + ports + `
`
}

func TestServiceSelector(t *testing.T) {
	web := deployment("checkout", "shop", "{app: checkout}", "ports: [{name: http, containerPort: 8080}]", "")

	res := check(t, web+service("checkout", "shop", "{app: checkout}", "[{port: 80, targetPort: http}]"))
	expect(t, one(t, res, "service-selector"), links.StatusOK, "")
	expect(t, one(t, res, "service-port"), links.StatusOK, "")
	if !res.Report.Consistent || res.Report.Status != links.VerdictPass {
		t.Errorf("report = %+v", res.Report)
	}

	// A typo matches nothing. The pods may be in the cluster, so this is
	// unresolved, and the report is not a pass.
	typo := web + service("checkout", "shop", "{app: checkuot}", "[{port: 80, targetPort: http}]")
	res = check(t, typo)
	l := one(t, res, "service-selector")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "Deployment/shop/checkout") || !strings.Contains(l.ClosestMatch, "app=checkout") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}
	if res.Report.Status != links.VerdictUnknown || res.Report.Complete || res.Report.Consistent {
		t.Errorf("an unresolved link passed: %+v", res.Report)
	}

	// With a complete bundle it is certain.
	res = check(t, typo, complete)
	l = one(t, res, "service-selector")
	expect(t, l, links.StatusBroken, links.ReasonNotInBundle)
	if strings.Contains(l.Message, "may already exist") {
		t.Errorf("message still hedges: %s", l.Message)
	}
	if res.Report.Status != links.VerdictFail {
		t.Errorf("status = %s", res.Report.Status)
	}

	// Pods in another namespace do not count, and the hint says so.
	res = check(t, web+service("checkout", "other", "{app: checkout}", "[{port: 80}]"))
	l = one(t, res, "service-selector")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "another namespace") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// No selector: nothing to check.
	res = check(t, "apiVersion: v1\nkind: Service\nmetadata: {name: ext}\nspec: {type: ExternalName, externalName: db.example.com}\n")
	expect(t, one(t, res, "service-selector"), links.StatusSkipped, links.ReasonNotApplicable)

	// A label only running pods have cannot be judged.
	res = check(t, web+service("checkout", "shop", "{pod-template-hash: abc123}", "[{port: 80}]"))
	expect(t, one(t, res, "service-selector"), links.StatusSkipped, links.ReasonRuntimeLabels)
}

func TestServicePorts(t *testing.T) {
	web := deployment("web", "", "{app: web}", "ports: [{name: http, containerPort: 8080}]", "")
	for name, tc := range map[string]struct {
		workload, ports string
		status          links.Status
		reason          links.Reason
	}{
		"name declared":       {web, "[{port: 80, targetPort: http}]", links.StatusOK, ""},
		"name missing":        {web, "[{port: 80, targetPort: htpp}]", links.StatusBroken, links.ReasonPortMissing},
		"number declared":     {web, "[{port: 80, targetPort: 8080}]", links.StatusOK, ""},
		"number not declared": {web, "[{port: 80, targetPort: 9090}]", links.StatusUnresolved, links.ReasonPortNotDeclared},
		"defaults to port":    {web, "[{port: 8080}]", links.StatusOK, ""},
		"no ports declared":   {deployment("web", "", "{app: web}", "", ""), "[{port: 80, targetPort: 9090}]", links.StatusSkipped, links.ReasonPortNotDeclared},
	} {
		t.Run(name, func(t *testing.T) {
			// A complete bundle must not turn an undeclared number into broken.
			res := check(t, tc.workload+service("web", "", "{app: web}", tc.ports), complete)
			l := one(t, res, "service-port")
			expect(t, l, tc.status, tc.reason)
			if name == "name missing" && !strings.Contains(l.ClosestMatch, "http") {
				t.Errorf("closest match = %q", l.ClosestMatch)
			}
		})
	}
}

// A named port that only some of the selected workloads declare may be
// intended, so it is not called broken.
func TestNamedPortOnSomeOfThePods(t *testing.T) {
	with := deployment("a", "", "{app: web}", "ports: [{name: metrics, containerPort: 9090}]", "")
	without := deployment("b", "", "{app: web}", "ports: [{name: http, containerPort: 8080}]", "")
	res := check(t, with+without+service("web", "", "{app: web}", "[{port: 9090, targetPort: metrics}]"), complete)
	got := map[string]string{}
	for _, l := range res.Links {
		if l.Check == "service-port" {
			got[l.To.Name] = string(l.Status) + "/" + string(l.Reason)
		}
	}
	if got["a"] != "ok/" || got["b"] != "unresolved/port_not_declared" {
		t.Errorf("links = %v", got)
	}
}

func TestMatchExpressionsUseTheKubernetesLibrary(t *testing.T) {
	pods := deployment("a", "", "{app: a, tier: web}", "", "") + deployment("b", "", "{app: b, tier: db}", "", "")
	pdb := func(selector string) string {
		return "\n---\napiVersion: policy/v1\nkind: PodDisruptionBudget\nmetadata: {name: p}\nspec:\n  minAvailable: 1\n  selector: " + selector + "\n"
	}
	targets := func(res *Result) string {
		var names []string
		for _, l := range res.Links {
			if l.Check == "pdb-selector" && l.Status == links.StatusOK {
				names = append(names, l.To.Name)
			}
		}
		return strings.Join(names, ",")
	}
	for selector, want := range map[string]string{
		`{matchExpressions: [{key: tier, operator: In, values: [web, cache]}]}`:            "a",
		`{matchExpressions: [{key: tier, operator: NotIn, values: [web]}]}`:                "b",
		`{matchExpressions: [{key: tier, operator: Exists}]}`:                              "a,b",
		`{matchLabels: {app: a}, matchExpressions: [{key: tier, operator: DoesNotExist}]}`: "",
	} {
		if got := targets(check(t, pods+pdb(selector))); got != want {
			t.Errorf("%s matched %q, want %q", selector, got, want)
		}
	}
	// An empty selector picks every pod, so there is nothing to get wrong.
	expect(t, one(t, check(t, pods+pdb("{}")), "pdb-selector"), links.StatusSkipped, links.ReasonNotApplicable)
	// A selector that is not valid is left to the schema check.
	expect(t, one(t, check(t, pods+pdb(`{matchExpressions: [{key: tier, operator: Sometimes}]}`)), "pdb-selector"), links.StatusSkipped, links.ReasonInvalid)

	np := "\n---\napiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: netpol}\nspec:\n  podSelector: {matchLabels: {app: zzz}}\n"
	expect(t, one(t, check(t, pods+np), "networkpolicy-selector"), links.StatusUnresolved, links.ReasonNotInBundle)
}

func TestWorkloadSelectorMustMatchItsTemplate(t *testing.T) {
	bad := `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: wbe}}
    spec: {containers: [{name: c, image: x}]}
`
	l := one(t, check(t, bad), "workload-selector")
	expect(t, l, links.StatusBroken, links.ReasonSelectorMismatch)
	if !strings.Contains(l.Message, "app=wbe") {
		t.Errorf("message does not show the labels: %s", l.Message)
	}
	expect(t, one(t, check(t, deployment("web", "", "{app: web}", "", "")), "workload-selector"), links.StatusOK, "")
}

const config = `
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {LOG_LEVEL: info}
binaryData: {logo.png: aGVsbG8=}
---
apiVersion: v1
kind: Secret
metadata: {name: db}
data: {password: c3VwZXItc2VjcmV0LXB3}
stringData: {token: tok-1234567890}
`

func TestConfigMapAndSecretReferences(t *testing.T) {
	for name, tc := range map[string]struct {
		container, pod string
		check          string
		status         links.Status
		reason         links.Reason
	}{
		"envFrom found":        {"envFrom: [{configMapRef: {name: settings}}]", "", "configmap", links.StatusOK, ""},
		"envFrom missing":      {"envFrom: [{secretRef: {name: nope}}]", "", "secret", links.StatusUnresolved, links.ReasonNotInBundle},
		"envFrom optional":     {"envFrom: [{secretRef: {name: nope, optional: true}}]", "", "secret", links.StatusSkipped, links.ReasonOptional},
		"key found":            {"env: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: LOG_LEVEL}}}]", "", "configmap", links.StatusOK, ""},
		"binary key found":     {"env: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: logo.png}}}]", "", "configmap", links.StatusOK, ""},
		"key missing":          {"env: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: LOG_LEVLE}}}]", "", "configmap", links.StatusBroken, links.ReasonKeyMissing},
		"key missing optional": {"env: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: x, optional: true}}}]", "", "configmap", links.StatusSkipped, links.ReasonOptional},
		"secret data key":      {"env: [{name: A, valueFrom: {secretKeyRef: {name: db, key: password}}}]", "", "secret", links.StatusOK, ""},
		"secret stringData":    {"env: [{name: A, valueFrom: {secretKeyRef: {name: db, key: token}}}]", "", "secret", links.StatusOK, ""},
		"secret key missing":   {"env: [{name: A, valueFrom: {secretKeyRef: {name: db, key: pasword}}}]", "", "secret", links.StatusBroken, links.ReasonKeyMissing},
		"key of missing":       {"env: [{name: A, valueFrom: {secretKeyRef: {name: nope, key: k}}}]", "", "secret", links.StatusUnresolved, links.ReasonNotInBundle},
		"volume found":         {"", "volumes: [{name: v, configMap: {name: settings}}]", "configmap", links.StatusOK, ""},
		"volume missing":       {"", "volumes: [{name: v, secret: {secretName: nope}}]", "secret", links.StatusUnresolved, links.ReasonNotInBundle},
		"projected missing":    {"", "volumes: [{name: v, projected: {sources: [{configMap: {name: nope}}]}}]", "configmap", links.StatusUnresolved, links.ReasonNotInBundle},
		"pull secret missing":  {"", "imagePullSecrets: [{name: nope}]", "secret", links.StatusUnresolved, links.ReasonNotInBundle},
		"cluster makes it":     {"", "volumes: [{name: v, configMap: {name: kube-root-ca.crt}}]", "configmap", links.StatusSkipped, links.ReasonClusterProvided},
	} {
		t.Run(name, func(t *testing.T) {
			expect(t, one(t, check(t, config+deployment("web", "", "{app: web}", tc.container, tc.pod)), tc.check), tc.status, tc.reason)
		})
	}

	// A volume item is checked as well as the object it comes from.
	res := check(t, config+deployment("web", "", "{app: web}", "", "volumes: [{name: v, secret: {secretName: db, items: [{key: nope, path: p}]}}]"))
	var statuses []string
	for _, l := range res.Links {
		if l.Check == "secret" {
			statuses = append(statuses, string(l.Status))
		}
	}
	if strings.Join(statuses, ",") != "ok,broken" {
		t.Errorf("volume item links = %v", statuses)
	}

	// The nearest key is suggested.
	l := one(t, check(t, config+deployment("web", "", "{app: web}", "env: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: LOG_LEVLE}}}]", "")), "configmap")
	if !strings.Contains(l.ClosestMatch, "LOG_LEVEL") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// An object with no keys is likely filled in by the cluster.
	empty := "\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: ca}\n"
	l = one(t, check(t, empty+deployment("web", "", "{app: web}", "env: [{name: A, valueFrom: {configMapKeyRef: {name: ca, key: ca.crt}}}]", ""), complete), "configmap")
	expect(t, l, links.StatusUnresolved, links.ReasonDetailsUnknown)
}

// Secret values are in the input. They must not come out again anywhere.
func TestSecretValuesAreNeverEchoed(t *testing.T) {
	res := check(t, config+deployment("web", "", "{app: web}",
		"env: [{name: A, valueFrom: {secretKeyRef: {name: db, key: pasword}}}, {name: B, valueFrom: {secretKeyRef: {name: db, key: token}}}]", ""),
		func(r *Request) { r.IncludeAll = true })
	out, err := json.Marshal(res.Report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, l := range res.Links {
		ex, err := res.Explain(l.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(ex)
		text += string(b)
	}
	for _, leak := range []string{"c3VwZXItc2VjcmV0LXB3", "super-secret-pw", "tok-1234567890"} {
		if strings.Contains(text, leak) {
			t.Errorf("output contains the secret value %q", leak)
		}
	}
	if !strings.Contains(text, "password") {
		t.Error("the key names should be listed")
	}
}

func TestClaimsAndServiceAccounts(t *testing.T) {
	pod := "serviceAccountName: runner\nvolumes: [{name: v, persistentVolumeClaim: {claimName: data}}]"
	res := check(t, deployment("web", "", "{app: web}", "", pod))
	expect(t, one(t, res, "pvc"), links.StatusUnresolved, links.ReasonNotInBundle)
	expect(t, one(t, res, "service-account"), links.StatusUnresolved, links.ReasonNotInBundle)

	have := "\n---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: data}\n---\napiVersion: v1\nkind: ServiceAccount\nmetadata: {name: runner}\n"
	res = check(t, have+deployment("web", "", "{app: web}", "", pod))
	expect(t, one(t, res, "pvc"), links.StatusOK, "")
	expect(t, one(t, res, "service-account"), links.StatusOK, "")

	// Every namespace has a default service account.
	res = check(t, deployment("web", "", "{app: web}", "", "serviceAccountName: default"), complete)
	expect(t, one(t, res, "service-account"), links.StatusSkipped, links.ReasonClusterProvided)
}

func TestClusterObjects(t *testing.T) {
	pod := "serviceAccountName: runner\nimagePullSecrets: [{name: regcred}]"
	existing := "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: runner}\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: regcred}\n" +
		// This one has a broken reference of its own. It must not be reported.
		deployment("old", "", "{app: old}", "envFrom: [{secretRef: {name: gone}}]", "")
	res := check(t, deployment("web", "", "{app: web}", "", pod)+service("old", "", "{app: old}", "[{port: 80}]"), inCluster(t, existing), complete)
	if !res.Report.Consistent {
		t.Errorf("report = %+v", res.Report)
	}
	l := one(t, res, "service-account")
	if l.To.Source != "clusterObjects" || !strings.Contains(l.Message, "supplied") {
		t.Errorf("link does not say the target came from the cluster: %+v", l)
	}
	expect(t, one(t, res, "service-selector"), links.StatusOK, "")
	if len(res.Report.Resources) != 2 {
		t.Errorf("cluster objects were listed as checked resources: %v", res.Report.Resources)
	}
}

func TestNamespaces(t *testing.T) {
	secret := func(ns string) string {
		if ns != "" {
			ns = ", namespace: " + ns
		}
		return "\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: db" + ns + "}\nstringData: {k: v}\n"
	}
	use := "envFrom: [{secretRef: {name: db}}]"

	// The same name in another namespace does not resolve, and the hint
	// names where it is.
	l := one(t, check(t, secret("data")+deployment("web", "shop", "{app: web}", use, "")), "secret")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "namespace data") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// One side with no namespace: not known, even for a complete bundle.
	res := check(t, secret("")+deployment("web", "shop", "{app: web}", use, ""), complete)
	expect(t, one(t, res, "secret"), links.StatusUnresolved, links.ReasonNamespaceUnknown)
	if len(res.Report.Notes) == 0 || !strings.Contains(res.Report.Notes[0], "set no namespace") {
		t.Errorf("notes = %v", res.Report.Notes)
	}

	// Saying where the bundle goes settles it.
	l = one(t, check(t, secret("")+deployment("web", "shop", "{app: web}", use, ""), func(r *Request) { r.Namespace = "shop" }), "secret")
	expect(t, l, links.StatusOK, "")

	// Neither side sets one: they are deployed together.
	res = check(t, secret("")+deployment("web", "", "{app: web}", use, ""))
	expect(t, one(t, res, "secret"), links.StatusOK, "")
	if len(res.Report.Notes) != 0 {
		t.Errorf("notes = %v", res.Report.Notes)
	}
}

func TestIngress(t *testing.T) {
	svc := service("web", "", "{app: web}", "[{name: http, port: 80, targetPort: 8080}]") + deployment("web", "", "{app: web}", "", "")
	ingress := func(svcName, port string) string {
		return `
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web}
spec:
  tls: [{hosts: [a.example.com], secretName: web-tls}]
  rules:
    - host: a.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: {service: {name: ` + svcName + `, port: ` + port + `}}
`
	}
	for name, tc := range map[string]struct {
		svcName, port string
		status        links.Status
		reason        links.Reason
	}{
		"by number":       {"web", "{number: 80}", links.StatusOK, ""},
		"by name":         {"web", "{name: http}", links.StatusOK, ""},
		"wrong number":    {"web", "{number: 8080}", links.StatusBroken, links.ReasonPortMissing},
		"wrong name":      {"web", "{name: https}", links.StatusBroken, links.ReasonPortMissing},
		"missing service": {"wbe", "{number: 80}", links.StatusUnresolved, links.ReasonNotInBundle},
	} {
		t.Run(name, func(t *testing.T) {
			res := check(t, svc+ingress(tc.svcName, tc.port))
			l := one(t, res, "ingress-backend")
			expect(t, l, tc.status, tc.reason)
			if name == "missing service" && !strings.Contains(l.ClosestMatch, "Service web") {
				t.Errorf("closest match = %q", l.ClosestMatch)
			}
			expect(t, one(t, res, "ingress-tls"), links.StatusUnresolved, links.ReasonNotInBundle)
		})
	}
}

func TestRoute(t *testing.T) {
	svc := service("web", "", "{app: web}", "[{name: http, port: 80, targetPort: 8080}]") + deployment("web", "", "{app: web}", "", "")
	route := func(to, port string) string {
		return "\n---\napiVersion: route.openshift.io/v1\nkind: Route\nmetadata: {name: web}\nspec:\n  to: {kind: Service, name: " + to + "}\n" + port
	}
	for name, tc := range map[string]struct {
		to, port string
		status   links.Status
		reason   links.Reason
	}{
		"no port":         {"web", "", links.StatusOK, ""},
		"port name":       {"web", "  port: {targetPort: http}\n", links.StatusOK, ""},
		"pod port number": {"web", "  port: {targetPort: 8080}\n", links.StatusOK, ""},
		"wrong name":      {"web", "  port: {targetPort: web}\n", links.StatusBroken, links.ReasonPortMissing},
		// A number that does not match is not called broken: the rule for
		// numbers has not been checked against a running router.
		"service port":    {"web", "  port: {targetPort: 80}\n", links.StatusUnresolved, links.ReasonPortNotDeclared},
		"missing service": {"nope", "  port: {targetPort: http}\n", links.StatusUnresolved, links.ReasonNotInBundle},
	} {
		t.Run(name, func(t *testing.T) {
			expect(t, one(t, check(t, svc+route(tc.to, tc.port)), "route-backend"), tc.status, tc.reason)
		})
	}
	// A Service that names its target port hides the number.
	named := service("web", "", "{app: web}", "[{name: http, port: 80, targetPort: http}]")
	expect(t, one(t, check(t, named+route("web", "  port: {targetPort: 9999}\n")), "route-backend"), links.StatusSkipped, links.ReasonDetailsUnknown)
}

func TestAutoscalerTarget(t *testing.T) {
	hpa := func(apiVersion, kind, name string) string {
		return "\n---\napiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata: {name: h}\nspec:\n  scaleTargetRef: {apiVersion: " + apiVersion + ", kind: " + kind + ", name: " + name + "}\n"
	}
	web := deployment("web", "", "{app: web}", "", "")
	expect(t, one(t, check(t, web+hpa("apps/v1", "Deployment", "web")), "hpa-target"), links.StatusOK, "")

	l := one(t, check(t, web+hpa("apps/v1", "StatefulSet", "web")), "hpa-target")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "Deployment/web") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// A custom workload is found by its group and kind, like any other.
	rollout := "\n---\napiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: web}\nspec:\n  template:\n    metadata: {labels: {app: web}}\n    spec: {containers: [{name: c, image: x, ports: [{name: http, containerPort: 80}]}]}\n"
	res := check(t, rollout+hpa("argoproj.io/v1alpha1", "Rollout", "web")+service("web", "", "{app: web}", "[{port: 80, targetPort: http}]"))
	expect(t, one(t, res, "hpa-target"), links.StatusOK, "")
	// Its pod template is read too, so a Service can select it.
	expect(t, one(t, res, "service-selector"), links.StatusOK, "")
	expect(t, one(t, res, "service-port"), links.StatusOK, "")
}

func TestRoleBindings(t *testing.T) {
	binding := func(kind, roleKind, roleName, subjects string) string {
		return "\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: " + kind + "\nmetadata: {name: b, namespace: shop}\nroleRef: {apiGroup: rbac.authorization.k8s.io, kind: " + roleKind + ", name: " + roleName + "}\nsubjects: " + subjects + "\n"
	}
	role := "\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata: {name: reader, namespace: shop}\n"
	sa := "\n---\napiVersion: v1\nkind: ServiceAccount\nmetadata: {name: app, namespace: shop}\n"

	res := check(t, role+sa+binding("RoleBinding", "Role", "reader", "[{kind: ServiceAccount, name: app}, {kind: User, name: jane}]"))
	expect(t, one(t, res, "binding-role"), links.StatusOK, "")
	expect(t, one(t, res, "binding-subject"), links.StatusOK, "")

	res = check(t, role+sa+binding("RoleBinding", "Role", "writer", "[{kind: ServiceAccount, name: app, namespace: other}]"))
	expect(t, one(t, res, "binding-role"), links.StatusUnresolved, links.ReasonNotInBundle)
	l := one(t, res, "binding-subject")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "namespace shop") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// The wrong kind of role is a hint, when the right name exists.
	l = one(t, check(t, role+binding("RoleBinding", "ClusterRole", "reader", "[]")), "binding-role")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "Role reader") {
		t.Errorf("closest match = %q", l.ClosestMatch)
	}

	// Built-in cluster roles do not need to be in the bundle.
	expect(t, one(t, check(t, binding("RoleBinding", "ClusterRole", "view", "[]"), complete), "binding-role"), links.StatusSkipped, links.ReasonClusterProvided)

	// A ClusterRoleBinding cannot use a Role at all.
	expect(t, one(t, check(t, role+binding("ClusterRoleBinding", "Role", "reader", "[]")), "binding-role"), links.StatusBroken, links.ReasonWrongKind)
}

func TestOtherReferences(t *testing.T) {
	sts := `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  serviceName: db-headless
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec: {containers: [{name: c, image: x}]}
`
	expect(t, one(t, check(t, sts), "statefulset-service"), links.StatusUnresolved, links.ReasonNotInBundle)

	// A Job's pods carry its name, so a Service may select them by it.
	job := "apiVersion: batch/v1\nkind: Job\nmetadata: {name: load}\nspec:\n  template:\n    spec: {restartPolicy: Never, containers: [{name: c, image: x}]}\n"
	expect(t, one(t, check(t, job+service("load", "", "{job-name: load}", "[{port: 80}]")), "service-selector"), links.StatusOK, "")

	// A CronJob's pod template is nested one level deeper.
	cron := "apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: nightly}\nspec:\n  schedule: '0 0 * * *'\n  jobTemplate:\n    spec:\n      template:\n        spec: {restartPolicy: Never, containers: [{name: c, image: x, envFrom: [{configMapRef: {name: nope}}]}]}\n"
	l := one(t, check(t, cron), "configmap")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.HasPrefix(l.Field, "spec.jobTemplate.spec.template.spec.containers[0]") {
		t.Errorf("field = %s", l.Field)
	}
}

// A custom resource that shares a kind name with a built-in one is not
// mistaken for it.
func TestKindsAreComparedWithTheirGroup(t *testing.T) {
	knative := "apiVersion: serving.knative.dev/v1\nkind: Service\nmetadata: {name: web}\nspec:\n  template:\n    spec: {containers: [{image: x}]}\n"
	ingress := "\n---\napiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: web}\nspec:\n  defaultBackend: {service: {name: web, port: {number: 80}}}\n"
	res := check(t, knative+ingress)
	expect(t, one(t, res, "ingress-backend"), links.StatusUnresolved, links.ReasonNotInBundle)
	for _, l := range res.Links {
		if l.Check == "service-selector" {
			t.Errorf("a Knative Service was checked as a core Service: %+v", l)
		}
	}
}

// Fields of the wrong type are the schema check's business. They must not
// stop this one or make it panic.
func TestMalformedManifestsDoNotPanic(t *testing.T) {
	res := check(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: a}
spec: just a string
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: b}
spec:
  selector: 7
  template:
    metadata: {labels: [1, 2]}
    spec:
      volumes: {not: a list}
      containers: [5, {name: c, env: [3, {valueFrom: 9}], envFrom: "x", ports: [{containerPort: "http"}]}]
---
apiVersion: v1
kind: Service
metadata: {name: s}
spec:
  selector: {app: [1]}
  ports: [{port: "eighty"}, 4]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: i}
spec: {rules: [3, {http: {paths: [{backend: {service: 5}}]}}], tls: "no"}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: r}
roleRef: "nope"
subjects: [1]
`)
	for _, l := range res.Links {
		if l.Status == links.StatusError {
			t.Errorf("error link: %+v", l)
		}
	}
	expect(t, one(t, res, "service-selector"), links.StatusSkipped, links.ReasonInvalid)
}

func TestDuplicatesAreNoted(t *testing.T) {
	cm := "\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata: {k: v}\n"
	res := check(t, cm+cm)
	found := false
	for _, n := range res.Report.Notes {
		found = found || strings.Contains(n, "more than once")
	}
	if !found {
		t.Errorf("notes = %v", res.Report.Notes)
	}
}

func TestRunStopsWhenOutOfTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Request{Resources: parse(t, deployment("web", "", "{app: web}", "", ""), "inline")}); err == nil {
		t.Error("a cancelled check returned a result")
	}
}

func TestFindingsAndExplain(t *testing.T) {
	res := check(t, config+deployment("web", "", "{app: web}", "envFrom: [{configMapRef: {name: settings}}, {secretRef: {name: nope}}]", ""))
	// ok links are counted, but only listed on request.
	if len(res.Report.Findings) != 1 || res.Report.Summary.OK != 2 || res.Report.Summary.Links != 3 {
		t.Errorf("findings = %d, summary = %+v", len(res.Report.Findings), res.Report.Summary)
	}
	f := res.Report.Findings[0]
	if !strings.HasPrefix(f.ID, "f-"+res.Report.CheckID+"-") {
		t.Errorf("id = %s", f.ID)
	}
	ex, err := res.Explain(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Expected == "" || ex.Found == "" || !strings.Contains(ex.Meaning, "not a pass") || len(ex.Candidates) != 1 {
		t.Errorf("explanation = %+v", ex)
	}
	if _, err := res.Explain("f-00000000-001"); err == nil {
		t.Error("an unknown id was explained")
	}
}

func TestSelect(t *testing.T) {
	all := len(Names())
	for name, tc := range map[string]struct {
		only, skip []string
		run        int
		err        string
	}{
		"everything":     {nil, nil, all, ""},
		"only one":       {[]string{"configmap"}, nil, 1, ""},
		"skip one":       {nil, []string{"hpa-target"}, all - 1, ""},
		"wildcard":       {[]string{"ingress-*"}, nil, 2, ""},
		"only then skip": {[]string{"binding-*"}, []string{"binding-subject"}, 1, ""},
		"mistyped":       {nil, []string{"hpa-taget"}, 0, "no check is named"},
		"mistyped only":  {[]string{"nope"}, nil, 0, "no check is named"},
		"nothing left":   {nil, []string{"*"}, 0, "leaves no check"},
		"bad pattern":    {[]string{"[a"}, nil, 0, "not a valid"},
	} {
		sel, err := Select(tc.only, tc.skip)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v", name, err)
			}
			continue
		}
		if err != nil || len(sel.Run()) != tc.run || len(sel.Run())+len(sel.Skipped()) != all {
			t.Errorf("%s: run %v, skipped %v, err %v", name, sel.Run(), sel.Skipped(), err)
		}
	}
}

func TestRegistryIsComplete(t *testing.T) {
	for _, c := range List() {
		if c.Description == "" || c.Decides == "" || len(c.Kinds) == 0 {
			t.Errorf("%s is missing a description, what it decides, or its kinds", c.Name)
		}
	}
}

func TestChecksCanBeTurnedOff(t *testing.T) {
	// One bundle with a broken key, a selector that matches nothing and a
	// missing port.
	bundle := config +
		deployment("web", "", "{app: web}", "ports: [{name: http, containerPort: 8080}]\nenv: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: nope}}}]", "") +
		service("web", "", "{app: web}", "[{port: 80, targetPort: htpp}]") +
		service("other", "", "{app: other}", "[{port: 80}]")
	names := func(res *Result) string {
		seen := map[string]bool{}
		for _, l := range res.Links {
			seen[l.Check] = true
		}
		return strings.Join(sortedKeys(seen), ",")
	}

	res := check(t, bundle)
	if got := names(res); got != "configmap,service-port,service-selector,workload-selector" {
		t.Fatalf("checks with links = %s", got)
	}
	if len(res.Report.ChecksSkipped) != 0 || len(res.Report.ChecksRun) != len(Names()) {
		t.Errorf("run %v, skipped %v", res.Report.ChecksRun, res.Report.ChecksSkipped)
	}

	// A check that is off leaves no links, and the report says it was off.
	res = check(t, bundle, func(r *Request) { r.SkipChecks = []string{"configmap", "service-selector"} })
	if got := names(res); got != "service-port,workload-selector" {
		t.Errorf("checks with links = %s", got)
	}
	if strings.Join(res.Report.ChecksSkipped, ",") != "service-selector,configmap" {
		t.Errorf("skipped = %v", res.Report.ChecksSkipped)
	}
	if len(res.Report.Notes) == 0 || !strings.Contains(strings.Join(res.Report.Notes, " "), "turned off") {
		t.Errorf("notes = %v", res.Report.Notes)
	}
	// The port check needs the selector's matches. It still has them.
	expect(t, one(t, res, "service-port"), links.StatusBroken, links.ReasonPortMissing)

	// The verdict covers what ran.
	res = check(t, bundle, func(r *Request) { r.Checks = []string{"workload-selector"} })
	if !res.Report.Consistent || res.Report.Summary.Links != 1 || len(res.Report.ChecksRun) != 1 {
		t.Errorf("report = %+v", res.Report)
	}

	if _, err := Run(context.Background(), Request{Resources: parse(t, bundle, "inline"), SkipChecks: []string{"confgimap"}}); err == nil {
		t.Error("a mistyped check name was accepted")
	}
}

// A failure inside a check is reported even when the name it would carry
// is turned off, and a link with a name that is not registered is a
// failure.
func TestFailuresAreNeverHidden(t *testing.T) {
	sel, err := Select(nil, []string{"service-selector"})
	if err != nil {
		t.Fatal(err)
	}
	c := &run{sel: sel}
	c.add(links.Link{Check: "service-selector", Status: links.StatusUnresolved})
	c.add(links.Link{Check: "service-selector", Status: links.StatusError})
	if len(c.links) != 1 || c.links[0].Status != links.StatusError {
		t.Errorf("links = %+v", c.links)
	}
	defer func() {
		if recover() == nil {
			t.Error("an unregistered check name was accepted")
		}
	}()
	c.add(links.Link{Check: "no-such-check", Status: links.StatusOK})
}

// A hint must point at the likeliest slip, and must not be offered when
// nothing is close.
func TestClosestMatchPicksTheLikelySlip(t *testing.T) {
	// Swapped letters are one edit, so "wbe" is nearer to "web" than to "db".
	for _, tc := range []struct {
		a, b string
		want int
	}{{"wbe", "web", 1}, {"web", "db", 2}, {"checkuot", "checkout", 1}, {"htpp", "http", 1}, {"same", "same", 0}, {"", "ab", 2}} {
		if got := distance(tc.a, tc.b); got != tc.want {
			t.Errorf("distance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	// A short name gets little slack: "db" is not a typo of "web".
	if n, ok := nearest("web", []string{"db"}); ok {
		t.Errorf("nearest(web, [db]) = %q, want none", n)
	}
	if n, ok := nearest("wbe", []string{"db", "web"}); !ok || n != "web" {
		t.Errorf("nearest(wbe, [db web]) = %q, %v", n, ok)
	}
	if n, ok := nearest("payment_url", []string{"payment-url", "timeout"}); !ok || n != "payment-url" {
		t.Errorf("nearest(payment_url) = %q, %v", n, ok)
	}

	// A selector with a typo names the workload it meant, not the one that
	// sorts first.
	bundle := deployment("web", "", "{app: web}", "", "") + deployment("db", "", "{app: db}", "", "") +
		service("front", "", "{app: wbe}", "[{port: 80}]")
	l := one(t, check(t, bundle), "service-selector")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "Deployment/web") {
		t.Errorf("closest match = %q, want the Deployment with app=web", l.ClosestMatch)
	}

	// An autoscaler that names the wrong kind is told so, even when another
	// workload of the kind it asked for exists.
	hpa := "\n---\napiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata: {name: h}\nspec:\n  scaleTargetRef: {apiVersion: apps/v1, kind: StatefulSet, name: web}\n"
	statefulSet := "\n---\napiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: db}\nspec:\n  selector: {matchLabels: {app: db}}\n  template:\n    metadata: {labels: {app: db}}\n    spec: {containers: [{name: c, image: x}]}\n"
	l = one(t, check(t, deployment("web", "", "{app: web}", "", "")+statefulSet+hpa), "hpa-target")
	expect(t, l, links.StatusUnresolved, links.ReasonNotInBundle)
	if !strings.Contains(l.ClosestMatch, "Deployment/web") || strings.Contains(l.ClosestMatch, "db") {
		t.Errorf("closest match = %q, want the Deployment named web", l.ClosestMatch)
	}

	// Messages use the right article.
	if article("Ingress") != "an" || article("Service") != "a" {
		t.Error("article is wrong")
	}
}

// An optional key that is absent never stops the pod, so it is skipped
// whether the object has other keys or none at all.
func TestOptionalKeyOfAnEmptyObject(t *testing.T) {
	pod := func(optional string) string {
		return "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n    - name: c\n      image: x\n      env:\n        - name: A\n          valueFrom: {secretKeyRef: {name: s, key: k" + optional + "}}\n"
	}
	empty := "\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: s}\n"
	other := "\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData: {another: x}\n"

	expect(t, one(t, check(t, pod(", optional: true")+empty), "secret"), links.StatusSkipped, links.ReasonOptional)
	expect(t, one(t, check(t, pod(", optional: true")+other), "secret"), links.StatusSkipped, links.ReasonOptional)
	// Without the flag the two cases still differ.
	expect(t, one(t, check(t, pod("")+empty), "secret"), links.StatusUnresolved, links.ReasonDetailsUnknown)
	expect(t, one(t, check(t, pod("")+other), "secret"), links.StatusBroken, links.ReasonKeyMissing)
}
