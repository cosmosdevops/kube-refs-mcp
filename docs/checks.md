# Checks

Each check follows one kind of reference. A reference that was followed is
called a link. Every link gets a status and, where it helps, a reason.

The `check` field of a finding holds one of these names. `kube-refs-mcp
checks` and the `list_checks` tool print the same list.

| Check | Looks at | What it decides |
| --- | --- | --- |
| `service-selector` | A Service's selector matches the pod template of a workload. | Unresolved when it matches nothing: matching pods may be in the cluster. |
| `service-port` | A Service's target port is a container port of the pods it selects. | Broken when a named port is declared by none of the selected pods. Unresolved when a number is not declared, because declaring ports is optional. |
| `workload-selector` | A workload's selector matches its own pod template labels. | Broken when it does not match. Both ends are in one resource, so this is always certain. |
| `configmap` | A pod's ConfigMap exists and has the keys the pod uses. | Broken when the ConfigMap is in the bundle without the key. Unresolved when the ConfigMap is not in the bundle. |
| `secret` | A pod's Secret exists and has the keys the pod uses. Covers image pull secrets. | Broken when the Secret is in the bundle without the key. Unresolved when the Secret is not in the bundle. |
| `pvc` | A pod's PersistentVolumeClaim exists. | Unresolved when the claim is not in the bundle. |
| `service-account` | A pod's ServiceAccount exists. | Unresolved when the ServiceAccount is not in the bundle. |
| `statefulset-service` | The Service a StatefulSet names in spec.serviceName exists. | Unresolved when the Service is not in the bundle. |
| `ingress-backend` | An Ingress backend's Service exists and exposes the port. | Broken when the Service is in the bundle without the port. Unresolved when the Service is not in the bundle. |
| `ingress-tls` | An Ingress's TLS Secret exists. | Unresolved when the Secret is not in the bundle. |
| `route-backend` | A Route's Service exists and has the port. | Broken when the Service is in the bundle without the port. Unresolved when the Service is not in the bundle. |
| `hpa-target` | The workload a HorizontalPodAutoscaler scales exists. | Unresolved when the workload is not in the bundle. |
| `pdb-selector` | A PodDisruptionBudget's selector matches the pod template of a workload. | Unresolved when it matches nothing. |
| `networkpolicy-selector` | A NetworkPolicy's pod selector matches the pod template of a workload. | Unresolved when it matches nothing. |
| `binding-role` | The role a RoleBinding or ClusterRoleBinding refers to exists. | Broken when a ClusterRoleBinding refers to a Role. Unresolved when the role is not in the bundle. |
| `binding-subject` | A ServiceAccount named as a subject of a binding exists. | Unresolved when the ServiceAccount is not in the bundle. |

Every check runs unless it is turned off. See
[Turning checks off](#turning-checks-off).

The sections below give the detail.

## Selectors

Selectors are parsed and matched with the Kubernetes library
(`k8s.io/apimachinery`), including `matchExpressions`. They behave here as
they do in a cluster.

A selector is compared with the pod templates in the same namespace: a Pod,
or the template of a Deployment, StatefulSet, DaemonSet, ReplicaSet,
ReplicationController, Job, CronJob or DeploymentConfig. Any other kind that
has a pod template at `spec.template` is read the same way.

| Check | From | Result |
| --- | --- | --- |
| `service-selector` | Service `spec.selector` | `ok` for each workload it selects. `unresolved` if it selects none. |
| `pdb-selector` | PodDisruptionBudget `spec.selector` | The same. |
| `networkpolicy-selector` | NetworkPolicy `spec.podSelector` | The same. |
| `workload-selector` | A workload's `spec.selector` | `broken` if it does not match the workload's own pod template. |

A selector that matches nothing is `unresolved`, not `broken`, because
matching pods may already run in the cluster. It becomes `broken` with
`bundleComplete`. The finding names the nearest pod template and says which
label differs.

`workload-selector` is always certain, because both ends are in the same
resource.

Skipped cases:

- A Service with no selector, or of type `ExternalName`.
- An empty selector on a PodDisruptionBudget or NetworkPolicy. It selects
  every pod, so there is nothing to get wrong.
- A selector that uses a label the cluster only adds to running pods, such
  as `pod-template-hash` or `statefulset.kubernetes.io/pod-name`.
- A selector that is not valid. The schema check reports that.

A Job's pods always carry `job-name` and `batch.kubernetes.io/job-name`, so
those two labels are added to a Job's template before matching.

## Service ports

`service-port` compares each `targetPort` of a Service with the container
ports of every workload the Service selects. With no `targetPort`, the
Service's `port` is used, as Kubernetes does.

| Target port | Pods | Result |
| --- | --- | --- |
| A name | Declare a port with that name | `ok` |
| A name | Do not, and no other selected workload does | `broken`. A named port only works if a container declares it. |
| A name | Do not, but another selected workload does | `unresolved`, reason `port_not_declared`. Those pods get no traffic on the port, which may be intended. |
| A number | Declare that number | `ok` |
| A number | Declare other ports | `unresolved`, reason `port_not_declared` |
| A number | Declare no ports | `skipped`, reason `port_not_declared` |

Kubernetes does not require a container to list the ports it listens on. A
number that is not listed may still work, so it is never called broken, not
even with `bundleComplete`.

Ports are read from containers and init containers.

## References from a pod

These are read from every pod template, in containers, init containers and
ephemeral containers.

| Check | Fields |
| --- | --- |
| `configmap` | `volumes[].configMap`, `volumes[].projected.sources[].configMap`, `envFrom[].configMapRef`, `env[].valueFrom.configMapKeyRef` |
| `secret` | `volumes[].secret`, `volumes[].projected.sources[].secret`, `envFrom[].secretRef`, `env[].valueFrom.secretKeyRef`, `imagePullSecrets[]` |
| `pvc` | `volumes[].persistentVolumeClaim.claimName` |
| `service-account` | `serviceAccountName` |
| `statefulset-service` | StatefulSet `spec.serviceName` |

A target that is not in the bundle is `unresolved`.

When a reference names a key (`configMapKeyRef`, `secretKeyRef`, or `items`
of a volume), and the object is in the bundle:

- The key is there: `ok`.
- The key is not there: `broken`. The pod would not start.
- The object lists no keys at all: `unresolved`, reason `details_unknown`.
  An empty ConfigMap or Secret is usually a placeholder that something in
  the cluster fills in.

Keys are read from `data`, `binaryData` and `stringData`. Only the key names
are read. A Secret of type `kubernetes.io/service-account-token` also counts
as having `token`, `ca.crt` and `namespace`.

A reference marked `optional: true` whose target or key is missing is
`skipped`.

## Ingress and Route

| Check | Fields |
| --- | --- |
| `ingress-backend` | Ingress `spec.defaultBackend` and `spec.rules[].http.paths[].backend`, and the older `serviceName` form |
| `ingress-tls` | Ingress `spec.tls[].secretName` |
| `route-backend` | Route `spec.to`, `spec.alternateBackends[]` and `spec.port.targetPort` |

A Service that is not in the bundle is `unresolved`. A Service that is in
the bundle but does not have the port is `broken`.

An Ingress port is a Service port, by number or by name.

A Route's `port.targetPort` is different. The OpenShift router compares it
with the Service's endpoints: a name is a Service port name, and a number is
the port on the pods, which is the Service's `targetPort`. A number is
`skipped` when the Service names its target ports, because the number behind
the name is on the pods. This rule is taken from how the router is
documented to behave. It has not been tested against a running router.

## Autoscaler

`hpa-target` looks up HorizontalPodAutoscaler `spec.scaleTargetRef` by API
group, kind and name. The target may be any kind, including a custom one.
If a workload with the same name but another kind is in the bundle, the
finding says so.

## Role bindings

| Check | Fields |
| --- | --- |
| `binding-role` | RoleBinding and ClusterRoleBinding `roleRef` |
| `binding-subject` | `subjects[]` of kind `ServiceAccount` |

A ClusterRoleBinding that refers to a `Role` is `broken`: Kubernetes does
not allow it. A role or service account that is not in the bundle is
`unresolved`. Users and groups are not objects, so they are not followed.

## Namespaces

Most references cannot name a namespace. A pod can only use a Secret in its
own namespace, an Ingress can only send to a Service in its own, and so on.
So a target is looked up in the namespace of the resource that refers to it.

If the name exists in the bundle in another namespace, the reference is
`unresolved` and the hint says where the name was found. It is not `broken`,
because an object of that name may also exist in the right namespace in the
cluster.

A resource with no `metadata.namespace` is a special case. The tool does not
know where it will be deployed.

- Pass `namespace`, and every such resource is treated as being in it.
- Otherwise, resources with no namespace are treated as being together,
  apart from every named namespace. A reference between a resource with a
  namespace and one without is `unresolved` with reason `namespace_unknown`.
  `bundleComplete` does not change that, because the gap is in what the tool
  was told, not in the bundle.

## Objects the cluster creates

A reference to one of these is `skipped` with reason `cluster_provided`:

- the `default` ServiceAccount
- the `kube-root-ca.crt` ConfigMap
- the ClusterRoles `cluster-admin`, `admin`, `edit` and `view`, and any
  ClusterRole whose name starts with `system:`

The list is short on purpose. It holds only what upstream Kubernetes always
creates. Anything else that your platform provides goes in `clusterObjects`.

## Turning checks off

Every check runs by default. A check can be turned off as a whole. There is
no way to hide a finding for one resource.

| Who | How | Lasts |
| --- | --- | --- |
| The caller | `checks` and `skipChecks` inputs of the tools | One call |
| The CLI user | `--check` and `--skip-check` | One run |
| The operator | `serve --skip-check` | Every call to that server |

`checks` and `--check` name the only checks to run. `skipChecks` and
`--skip-check` name checks to leave out. Both can be used together.

A name may use `*` as a wildcard, so `ingress-*` covers `ingress-backend`
and `ingress-tls`. Take care with `service-*`: it also covers
`service-account`.

A name that matches no check is an error, so a typing mistake cannot shrink
the check unnoticed. A selection that leaves nothing to run is an error too.

A check the operator turned off cannot be turned back on by a caller.
`list_checks` shows which those are, under `skippedByServer`.

A check that is off leaves no links. The report says so in three places:

- `checksRun` lists the checks that ran.
- `checksSkipped` lists the checks that were off.
- `notes` has a line naming them.

The verdict covers only the checks that ran. A report can be `consistent`
with checks turned off, so read `checksSkipped` before trusting it.

Some checks share work. `service-port` compares a Service's ports with the
pods its selector found. Turning off `service-selector` hides the selector
findings, and `service-port` still runs.

## Adding a check

The checks are listed in one table, in `internal/checks/registry.go`.

1. Add a row to `registry`: the name, what it looks at, the kinds, and what
   it decides.
2. Write the function, and add it to `runners` with the names it produces.
3. Add a test, and a row to the table at the top of this page.

A link with a name that is not in the table is refused, and a test compares
the table with this page, so the three cannot drift apart.

## What is not checked

- Whether a manifest is valid. Fields of the wrong type are ignored here.
- Policy. Nothing here says what you should do, only what will not work.
- NetworkPolicy peers under `ingress[].from` and `egress[].to`.
- Volume types other than `configMap`, `secret`, `projected` and
  `persistentVolumeClaim`, for example the secret of a `csi` volume.
- StorageClass, PriorityClass, IngressClass and RuntimeClass names.
- Gateway API routes.
- PersistentVolumeClaims that a StatefulSet makes from
  `volumeClaimTemplates`.
- Objects made by operators or admission webhooks.
- Duplicate resources. They are mentioned in the report's notes, and the
  later one is used.
