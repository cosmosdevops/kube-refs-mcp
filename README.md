# kube-refs-mcp

An MCP server that checks whether a set of Kubernetes manifests fits
together, so an AI coding agent can find broken references before anything
reaches a cluster.

A manifest can be valid and still not work with the ones next to it. A
Service whose selector has a typo passes a schema check and a policy check,
then routes to nothing. This tool follows the references between resources
and reports the ones that do not resolve:

- a Service to the pods it selects, and its target ports to their ports
- a pod to its ConfigMaps, Secrets and their keys, claims and service account
- an Ingress or Route to its Service and port
- an autoscaler to its workload
- a PodDisruptionBudget or NetworkPolicy to the pods it selects
- a role binding to its role and service accounts
- a workload's selector to its own pod template

It works offline. It does not contact a cluster and does not apply anything.

It does not check schemas or policies. It is meant to run between the two:
schema first (for example [ojs-mcp](https://github.com/cosmosdevops/ojs-mcp)),
then this, then policy (for example
[kube-policy-mcp](https://github.com/cosmosdevops/kube-policy-mcp)).

## Install

With Go 1.27 or newer:

```bash
go install github.com/cosmosdevops/kube-refs-mcp/cmd/kube-refs-mcp@latest
```

Or use the container image, `ghcr.io/cosmosdevops/kube-refs-mcp`. See
[docs/hosting.md](docs/hosting.md#using-the-image-locally).

## Try it

```bash
git clone https://github.com/cosmosdevops/kube-refs-mcp.git
cd kube-refs-mcp
make build
./bin/kube-refs-mcp check --dir examples/manifests/broken
```

```text
Verdict:   FAIL (complete: false, consistent: false)
Checked:   6 resource(s), 6 link(s)
Results:   2 broken, 2 unresolved, 0 error(s), 2 ok, 0 skipped

examples/manifests/broken/checkout.yaml
  ...
  Deployment/shop/checkout
    BROKEN      configmap -> ConfigMap/shop/checkout-config key payment_url
      Deployment/shop/checkout uses key "payment_url" of ConfigMap/shop/checkout-config, which does not have that key. The pod will not start.
      field: spec.template.spec.containers[0].env[0].valueFrom.configMapKeyRef.key
      closest match: key payment-url of ConfigMap/shop/checkout-config
      fix: Use a key the ConfigMap has, or add "payment_url" to it.
  Service/shop/checkout
    UNRESOLVED  service-selector -> Pod with labels app=checkuot
      Service/shop/checkout selects pods with labels app=checkuot. No pod template in the bundle has them in namespace "shop". Matching pods may already exist in the cluster.
      field: spec.selector
      closest match: Deployment/shop/checkout: it has app=checkout where the selector wants app=checkuot
      ...
```

## What the statuses mean

The tool never claims more than it can know. A reference to something that
is not in the bundle may be a mistake, or the object may already exist in
the cluster. The tool cannot tell, so it does not call that broken.

| Status | Meaning |
| --- | --- |
| `ok` | Both ends are known and they fit. |
| `broken` | Both ends are known and they do not fit. This is certain. |
| `unresolved` | The target is not in the bundle. It may exist in the cluster. Not a failure, and not a pass. |
| `skipped` | The check does not apply, or the kind is not understood. |
| `error` | The check itself failed. Not a pass. |

A report has a verdict, and says separately whether it is complete:

- `status` is `fail` if anything is broken, else `unknown` if anything is
  unresolved or errored, else `pass`.
- `complete` is `false` if anything is unresolved or errored, or if a file
  in a directory could not be read.
- `consistent` is `true` only when nothing is broken and the report is
  complete.

Two options settle an unresolved reference:

- `bundleComplete` says the bundle is everything that will exist. A missing
  target then counts as broken.
- `clusterObjects` lists objects that already exist in the cluster. They can
  be the target of a reference.

Every check runs by default. A whole check can be turned off, for one call
or for a whole server, and the report then lists it under `checksSkipped`.

[docs/results.md](docs/results.md) has a full result. [docs/checks.md](docs/checks.md)
lists every check, what it can and cannot decide, and how to turn one off.

## Use it with an agent

The server speaks MCP over stdio. For Claude Code:

```bash
claude mcp add kube-refs -- kube-refs-mcp serve --workspace-root .
```

Or in `.mcp.json`:

```json
{
  "mcpServers": {
    "kube-refs": {
      "command": "kube-refs-mcp",
      "args": ["serve", "--workspace-root", "."]
    }
  }
}
```

`--workspace-root` adds a directory the server may read manifests from. With
no such directory, callers send manifests inline. There is no config file.

| Tool | What it does |
| --- | --- |
| `check_manifests` | Checks inline YAML or JSON, or workspace files, as one bundle. |
| `check_directory` | Checks every manifest under a workspace directory as one bundle. |
| `describe_links` | Returns every reference found, including the ones that are fine. |
| `explain_finding` | Explains one finding: what was expected, what was found, what else exists. |
| `list_checks` | Lists the checks and what each one can decide. |

Send everything that is deployed together in one call. A reference can only
be followed to a resource that is in the same call.

## Command line

The CLI runs the same code as the MCP server.

```bash
kube-refs-mcp check --dir ./k8s
kube-refs-mcp check -f app.yaml -f config.yaml --namespace shop
kube-refs-mcp check --dir ./k8s --complete
kube-refs-mcp check --dir ./k8s --cluster-objects existing.yaml -o json
cat app.yaml | kube-refs-mcp check -f - --explain
kube-refs-mcp check --dir ./k8s --skip-check networkpolicy-selector
kube-refs-mcp links --dir ./k8s
kube-refs-mcp checks
```

`check` exits with `0` when consistent, `1` when a reference is broken, `2`
when nothing is broken but something is unresolved or a check failed, and
`3` on a usage or runtime error.

In CI, a bundle that relies on objects already in the cluster exits with
`2` until those objects are listed with `--cluster-objects`. Once they are,
add `--complete` so that a missing target fails the build.

## Hosting

`serve --http` runs one server for many agents. Each caller needs an API
key, and each key has its own rate limit.

```bash
kube-refs-mcp new-key alice >> keys
kube-refs-mcp serve --http 127.0.0.1:8089 --keys keys
```

[docs/hosting.md](docs/hosting.md) has the systemd and Docker set-ups, and
the files are in [deploy](deploy).

## Security

- No cluster client is in the program.
- MCP arguments are never passed to a shell.
- Files are read only inside the workspace roots. Symlinks are resolved
  before that check.
- Input size, file counts, resource counts, name lengths, the number of
  references and run time are bounded.
- Only the key names of a `Secret` or `ConfigMap` are read. Values are never
  copied into a result.
- A check that fails becomes an `error` finding, never a pass. A file in a
  directory that cannot be read makes the report incomplete.
- In hosted mode, API keys are stored only as SHA-256 hashes.

## Limitations

- Helm charts and Kustomize overlays are not rendered. Render them first and
  check the output.
- Nothing a cluster adds is simulated: defaults, generated names, or objects
  made by operators such as cert-manager or sealed-secrets. A Secret made
  that way is `unresolved` unless it is passed in `clusterObjects`.
- NetworkPolicy peers (`from` and `to`) are not followed. Only the policy's
  own pod selector is.
- Custom resources are handled in two generic ways only: a kind with a
  `spec.template` pod template is treated as a workload, and an autoscaler
  can target any kind.

[docs/checks.md](docs/checks.md#what-is-not-checked) has the full list.

## Documentation

| Page | Covers |
| --- | --- |
| [checks.md](docs/checks.md) | Every check, what it decides, turning checks off, adding one |
| [results.md](docs/results.md) | The report, the options, reasons, exit codes |
| [hosting.md](docs/hosting.md) | HTTP mode, API keys, systemd, Docker |
| [design.md](docs/design.md) | How it is built and why |

## Development

```bash
make test
make vet
```

The tests in [tests](tests) build the binary and drive it over MCP, the way
a client does. CI runs the same checks on every push and pull request, and
publishes the container image from `main`.

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

The licences of the open source modules built into the program are in
`/licenses` in the container image. `make licenses` produces the same set
for a binary you built yourself.

This project is not affiliated with or endorsed by the Kubernetes project
or Red Hat.
