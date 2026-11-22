# Results

## A report

`check_manifests` and `kube-refs-mcp check -o json` return this. It is cut
down here to two findings.

```json
{
  "checkId": "b1c8e8a3",
  "status": "fail",
  "complete": false,
  "consistent": false,
  "bundleComplete": false,
  "checksRun": ["service-selector", "service-port", "workload-selector", "configmap", "..."],
  "summary": {
    "resources": 6,
    "links": 6,
    "ok": 2,
    "broken": 2,
    "unresolved": 2,
    "skipped": 0,
    "errors": 0
  },
  "resources": [
    {"apiVersion": "v1", "kind": "ConfigMap", "namespace": "shop", "name": "checkout-config", "source": "examples/manifests/broken/checkout.yaml#0"}
  ],
  "findings": [
    {
      "id": "f-b1c8e8a3-002",
      "check": "configmap",
      "status": "broken",
      "reason": "key_missing",
      "from": {"apiVersion": "apps/v1", "kind": "Deployment", "namespace": "shop", "name": "checkout", "source": "examples/manifests/broken/checkout.yaml#2"},
      "field": "spec.template.spec.containers[0].env[0].valueFrom.configMapKeyRef.key",
      "to": {"kind": "ConfigMap", "namespace": "shop", "name": "checkout-config", "key": "payment_url", "source": "examples/manifests/broken/checkout.yaml#0"},
      "message": "Deployment/shop/checkout uses key \"payment_url\" of ConfigMap/shop/checkout-config, which does not have that key. The pod will not start.",
      "closestMatch": "key payment-url of ConfigMap/shop/checkout-config",
      "fix": "Use a key the ConfigMap has, or add \"payment_url\" to it."
    },
    {
      "id": "f-b1c8e8a3-004",
      "check": "service-selector",
      "status": "unresolved",
      "reason": "not_in_bundle",
      "from": {"apiVersion": "v1", "kind": "Service", "namespace": "shop", "name": "checkout", "source": "examples/manifests/broken/checkout.yaml#3"},
      "field": "spec.selector",
      "to": {"kind": "Pod", "namespace": "shop", "selector": "app=checkuot"},
      "message": "Service/shop/checkout selects pods with labels app=checkuot. No pod template in the bundle has them in namespace \"shop\". Matching pods may already exist in the cluster.",
      "closestMatch": "Deployment/shop/checkout: it has app=checkout where the selector wants app=checkuot",
      "fix": "Make spec.selector agree with the labels of the pods it is meant for. If those pods already exist in the cluster, pass their workload in clusterObjects."
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `status` | `fail` if any link is broken. Otherwise `unknown` if the report is not complete. Otherwise `pass`. |
| `complete` | `false` if any link is unresolved or errored, or if a file could not be read. |
| `consistent` | `true` only when nothing is broken and the report is complete. |
| `bundleComplete` | Repeats the option the caller set. |
| `checksRun` | The checks that ran. |
| `checksSkipped` | The checks that were turned off. Left out when there are none. |
| `summary` | Counts of every link, including the ones not listed. |
| `findings` | Links that are `broken`, `unresolved` or `error`. With `includeAll`, every link. |
| `skippedFiles` | Files, or documents in a file, that were found but not checked. See [Skipped files](#skipped-files). |
| `notes` | Assumptions that applied, such as how resources with no namespace were treated. |

A finding has:

| Field | Meaning |
| --- | --- |
| `id` | Pass it to `explain_finding`. |
| `check` | Which check made it. See [checks.md](checks.md). |
| `status`, `reason` | The outcome and why. |
| `from`, `field` | The resource that holds the reference, and where in it. |
| `to` | What the reference points at: kind, namespace and name, plus a `key`, `port` or `selector` where one applies. `source` is set when the target was found. |
| `closestMatch` | The nearest thing that does exist. A hint, not a certainty. |
| `fix` | What to change. |

`check_directory` returns the same report with the findings grouped under
`files`, by file and then by resource.

## Reasons

| Reason | Status | Meaning |
| --- | --- | --- |
| `not_in_bundle` | `unresolved`, or `broken` with `bundleComplete` | The target is not in the bundle or in `clusterObjects`. |
| `namespace_unknown` | `unresolved` | One end sets no namespace, so the two cannot be compared. |
| `port_not_declared` | `unresolved` or `skipped` | The pods do not list the port, and that is not certain to be a mistake. |
| `details_unknown` | `unresolved` or `skipped` | The target is known, but not its keys or ports. |
| `key_missing` | `broken` | The ConfigMap or Secret does not have the key. |
| `port_missing` | `broken` | The target does not have the port. For a named Service target port, only with `bundleComplete`. |
| `selector_mismatch` | `broken` | A workload's selector does not match its own pod template. |
| `wrong_kind` | `broken` | The reference names a kind that cannot be used there. |
| `optional` | `skipped` | The reference is marked optional and the target is absent. |
| `cluster_provided` | `skipped` | The cluster creates the target itself. |
| `runtime_labels` | `skipped` | The selector uses a label only running pods have. |
| `not_applicable` | `skipped` | There is nothing to check. |
| `invalid` | `unresolved` | The reference is not well-formed, or far longer than Kubernetes allows, so it could not be followed. |
| `check_failed` | `error` | The check stopped with an internal error. |

## Options

| MCP input | CLI flag | Effect |
| --- | --- | --- |
| `namespace` | `--namespace`, `-n` | The namespace the bundle is deployed to. Used for resources that set none. |
| `bundleComplete` | `--complete` | The bundle, with the cluster objects, is everything that will exist. A missing target is `broken`. |
| `clusterObjects` | `--cluster-objects FILE` | Objects that already exist in the cluster. |
| `includeAll` | `--all` | Also list links that are `ok` or `skipped`. |
| `checks` | `--check NAME` | Only run these checks. |
| `skipChecks` | `--skip-check NAME` | Leave these checks out. |

[checks.md](checks.md#turning-checks-off) explains how turning checks off
works and what the report then says.

### Cluster objects

A cluster object can be the target of a reference. Its own references are
not checked, and it is taken on trust.

Give each object the parts a reference needs:

- any object: `apiVersion`, `kind`, `metadata.name` and `metadata.namespace`
- a ConfigMap or Secret that is used by key: the keys under `data`. The
  values can be empty.
- a Service that an Ingress or Route sends to: `spec.ports`
- a workload that a Service selects: the pod template labels, and the
  container ports if the Service uses a named port

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: registry-credentials
  namespace: shop
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: platform-settings
  namespace: shop
data:
  region: ""
```

`bundleComplete` only changes links with the reason `not_in_bundle`. Links
that are open for another reason stay `unresolved`.

## describe_links

`describe_links` and `kube-refs-mcp links` return every link, under `links`
instead of `findings`, with the same fields. An `ok` link from a selector
names the workload whose pods were selected:

```text
Deployment/shop/checkout
  OK          workload-selector      -> Deployment/shop/checkout
  OK          configmap              -> ConfigMap/shop/checkout-config
  OK          secret                 -> Secret/shop/checkout-db key password
  OK          service-account        -> ServiceAccount/shop/checkout
Service/shop/checkout
  OK          service-selector       -> Deployment/shop/checkout
  OK          service-port           -> Deployment/shop/checkout port http
Ingress/shop/checkout
  OK          ingress-backend        -> Service/shop/checkout port 80
```

## explain_finding

`explain_finding` takes the `id` of a finding or a link and returns:

| Field | Meaning |
| --- | --- |
| `finding` | The finding itself. |
| `summary` | Its message. |
| `meaning` | What the status claims and what it does not. |
| `expected` | What the reference needs. |
| `found` | What is there instead. |
| `candidates` | What does exist where the target was looked for: names, keys, ports, or pod template labels. |
| `fix` | What to change. |

The server keeps the links of the last 50 checks of a session for this, or
fewer when the checks are large. It does not keep the manifests. Over HTTP,
each API key holder has their own history.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Consistent. |
| `1` | At least one reference is broken. |
| `2` | Nothing is broken, but something is unresolved or a check failed. |
| `3` | Usage or runtime error. |

`kube-refs-mcp links` does not give a verdict. It exits `0` unless the
bundle cannot be read.

## Limits

| Flag | Default | Bounds |
| --- | --- | --- |
| `--max-manifest-bytes` | 4194304 | One inline input, or one file |
| `--max-resources` | 2000 | Resources in one check |
| `--max-files` | 2000 | Manifest files under one directory |
| `--timeout` | 30s | One check |

Some limits are fixed:

| Limit | Value |
| --- | --- |
| A resource's name, namespace, kind or `apiVersion`, and any field name | 512 characters |
| References followed in one check | 20000 |
| A YAML document once its aliases are written out | `--max-manifest-bytes` |

Kubernetes allows at most 253 characters for a name, so the first limit
does not get in the way of a real manifest.

A request over a limit is refused with an error. It is never cut short and
reported as a pass. In a directory check, a file over a limit is listed as
not read, and the report is then not complete.

A reference inside a resource that is far longer than Kubernetes allows is
not followed. It is `unresolved` with reason `invalid`.

## Skipped files

A directory check lists what it found but did not check under
`skippedFiles`. Each entry has a `path` and a `reason`.

| Case | `notRead` | Effect |
| --- | --- | --- |
| A file with no Kubernetes resource, such as a Helm values file | no | None. |
| A document that is not a resource, in a file with others, such as a Kustomization | no | None. The other documents in the file are checked. |
| A file that is not valid YAML or JSON, such as a Helm template | `true` | The report is not complete. |
| A file over a limit | `true` | The report is not complete. |
| A symbolic link | `true` | The report is not complete. Links are never followed. |

What a file that was not read holds is unknown, so the tool does not report
a pass over it. The command line marks these files `NOT READ`.

A file named directly, with `files` or `--file`, is different. It must be
a manifest all the way through, or the request is refused.
