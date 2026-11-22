# Rules: checks for your own kinds

The built-in checks know core Kubernetes and OpenShift. They do not know how
the custom resources on your platform refer to each other: that a
`Certificate` names an `Issuer`, or that an `HTTPRoute` names a `Service`.

A rule tells the tool about one such reference. Rules are written in YAML and
loaded with `--rules`.

```bash
kube-refs-mcp check --rules rules/gateway-api.yaml -f examples/rules/gateway.yaml
```

```text
Verdict:   FAIL (complete: false, consistent: false)
Checked:   3 resource(s), 6 link(s)
Results:   1 broken, 1 unresolved, 0 error(s), 4 ok, 0 skipped

examples/rules/gateway.yaml
  HTTPRoute/shop/shop
    UNRESOLVED  httproute-parent -> Gateway/infra/edge
      HTTPRoute/shop/shop refers to Gateway "edge" in namespace "infra", which is not in the bundle. It may already exist in the cluster.
    BROKEN      httproute-backend -> Service/shop/web port 8080
      HTTPRoute/shop/shop sends to port 8080 of Service/shop/web, which does not expose that port.
      closest match: Service/shop/web exposes 80 (http)
```

Without the rule, the same bundle passes, because the tool does not know
what an `HTTPRoute` points at.

## A rule only points at a reference

A rule says where a reference is. It does not decide the result. The tool
works out the status with the same code as for the built-in checks:

- the target is in the bundle: `ok`
- the target is not in the bundle: `unresolved`
- the target is there without the port or key asked for: `broken`

So a rule cannot report a pass that was not established, and
`bundleComplete`, `clusterObjects`, the hints and `explain_finding` all work
for it.

There is no way to put logic in a rule. If you need to say what a manifest
should do, not what it refers to, that is a policy and belongs in a policy
tool.

## The rule library

[rules](../rules) has rules for some common operators. None is on by
default.

| File | Checks |
| --- | --- |
| `cert-manager.yaml` | `certificate-issuer` |
| `gateway-api.yaml` | `gateway-class`, `gateway-certificate`, `httproute-parent`, `httproute-backend` |
| `prometheus-operator.yaml` | `servicemonitor-selector`, `podmonitor-selector` |
| `external-secrets.yaml` | `externalsecret-store` |
| `keda.yaml` | `scaledobject-target` |

Load one file, several, or the whole directory:

```bash
kube-refs-mcp serve --rules rules/gateway-api.yaml --rules rules/cert-manager.yaml
```

```bash
kube-refs-mcp serve --rules rules
```

The container image has the library at `/rules`.

These rules were written from each project's API reference. They are tested
against sample manifests, not against the running operators.

## Writing a rule

A rules file has a list of checks. Each one names the kind it reads and
describes either a reference by name or a reference by labels.

### A reference by name

```yaml
checks:
  - name: certificate-issuer
    description: The issuer a Certificate names exists.
    from: {group: cert-manager.io, kind: Certificate}
    reference:
      name: spec.issuerRef.name
      to:
        group: cert-manager.io
        kind: Issuer
```

| Field | Meaning |
| --- | --- |
| `name` | The name of the check. Lower-case letters, digits and dashes. It must not be the name of a built-in check or of another rule. |
| `description` | One sentence, shown by `list_checks`. Written for you if left out. |
| `from` | The kind the reference is read from: `group` and `kind`. Leave `group` out for the core group. |
| `reference.name` | The path to the field that holds the target's name. |
| `reference.to` | The kind of the target. See below. |

A path is field names joined by dots, as in `spec.issuerRef.name`. A field
name that itself contains a dot, such as an annotation key, cannot be
written.

### The target

```yaml
      to:
        group: cert-manager.io
        kind: Issuer
        kindField: spec.issuerRef.kind
        groupField: spec.issuerRef.group
        clusterScoped: [ClusterIssuer]
```

| Field | Meaning |
| --- | --- |
| `group`, `kind` | The target's group and kind, used when the manifest does not say otherwise. |
| `kindField` | A path to a field that names the kind. When the manifest sets it, it wins over `kind`. |
| `groupField` | The same for the group. A field that is set to an empty string means the core group. |
| `apiVersionField` | A path to an `apiVersion` field. The group is taken from it. Use this or `groupField`, not both. |
| `clusterScoped` | Target kinds that have no namespace. The tool already knows the built-in ones. |

A target is looked up in the namespace of the resource that refers to it.
Add `namespace` when the reference can name another one:

```yaml
    reference:
      name: name
      namespace: namespace
```

### A list of references

`each` reads the reference once from every item of a list. The other paths
are then relative to the item.

```yaml
  - name: httproute-backend
    from: {group: gateway.networking.k8s.io, kind: HTTPRoute}
    reference:
      each: spec.rules[].backendRefs[]
      name: name
      namespace: namespace
      port: port
      to:
        kind: Service
        kindField: kind
        groupField: group
```

`each` ends in `[]` and may pass through other lists. The finding names the
exact item, for example `spec.rules[0].backendRefs[1].port`.

### A port or a key

| Field | Meaning |
| --- | --- |
| `port` | A path to a port number or name. The target must be a Service. A Service in the bundle that does not expose the port is `broken`. |
| `key` | A path to a key. The target must be a ConfigMap or a Secret. A target in the bundle without the key is `broken`. Only key names are read, never values. |

Use one or the other. When the target turns out to be some other kind, the
port or key is not compared.

### A reference that must be there

```yaml
    reference:
      name: spec.issuerRef.name
      required: true
```

By default a resource that does not set the field is `skipped`, with a
message that the rule found nothing to follow. That message matters for a
kind that has no schema to check: a misspelt field name would otherwise
leave no trace.

With `required: true`, a resource that does not set the field is `broken`,
with the reason `reference_missing`. Use it only where the resource cannot
work without the reference.

### A reference by labels

```yaml
  - name: servicemonitor-selector
    from: {group: monitoring.coreos.com, kind: ServiceMonitor}
    unlessSet: spec.namespaceSelector
    selector:
      field: spec.selector
      matches: {kind: Service}
```

| Field | Meaning |
| --- | --- |
| `selector.field` | The path to the selector. It may be a plain label map, or an object with `matchLabels` and `matchExpressions`. |
| `selector.matches` | The kind whose labels are compared. Leave it out to compare with pod templates, as a Service's selector is. |

Selectors are matched with the Kubernetes library, in the namespace of the
resource. A selector that matches nothing is `unresolved`.

### When a rule should step aside

`unlessSet` names a field that changes what the reference means. A resource
that sets it is `skipped`. In the example above, a ServiceMonitor with a
`namespaceSelector` looks in other namespaces, which the rule does not
follow, so the rule does not judge it.

## Loading and managing rules

- `--rules` takes a file or a directory and can be repeated. It works for
  every command: `check`, `links`, `checks` and `serve`.
- A rules file that is wrong stops the program, with the file and the rule
  named. A rule is never left out quietly.
- `kube-refs-mcp checks --rules ...` and the `list_checks` tool show each
  rule with `source: rule` and the file it came from.
- A rule is turned off like any other check, with `--skip-check` or
  `skipChecks`. Wildcards work: `httproute-*`.
- Only the operator of a server adds rules, when it starts. A caller cannot
  send rules with a request.

## What rules cannot do

- Follow a reference inside free text, such as a host name in a URL.
- Read a field whose name contains a dot.
- Resolve a name that needs working out, such as a name plus a suffix.
- Judge a condition on a reference. A Gateway API reference to another
  namespace is followed, but whether a `ReferenceGrant` allows it is not.
- Tell that a field is misspelt, beyond saying that nothing was found. A
  schema check does that, for kinds that have a schema.

For a reference that needs real logic, add a built-in check. See
[checks.md](checks.md#adding-a-check).
