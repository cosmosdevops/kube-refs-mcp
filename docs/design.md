# Design

## What it is for

Three questions can be asked of a set of manifests before it is deployed:

1. Is each one well-formed? A schema validator answers that.
2. Do they fit together? This tool answers that.
3. Are they allowed? A policy engine answers that.

The checks here are facts about how Kubernetes wires resources together.
They are the same for every organisation. So there are no policy packs, no
exemptions and no config file. A whole check can be turned off, and the
report then says so, but a finding cannot be hidden for one resource. If a finding would mean "not allowed"
rather than "does not work", it belongs in a policy tool.

## Never claim more than can be known

The tool only sees the bundle. It does not see the cluster.

A reference to an object that is not in the bundle may be a mistake. Or the
object may already exist in the cluster. The tool cannot tell these apart,
and a tool that guesses is one that agents learn to ignore. So:

- `broken` is used only when both ends of a link are known and do not fit.
- A missing target is `unresolved`.
- An `unresolved` link is never counted as a pass. The verdict is `unknown`
  and `complete` is `false`.

The caller can supply what the tool is missing: `clusterObjects` for what
already exists, and `bundleComplete` to say there is nothing else.

The same rule decides the smaller cases. A numeric `targetPort` that the
pods do not list may still work, so it is not broken. An empty ConfigMap
may be filled in by the cluster, so a key of it is not broken. A name found
in another namespace may also exist in the right one, so that is not broken
either.

## Layout

```text
cmd/kube-refs-mcp      main
internal/app           limits, time limit, version
internal/manifests     parsing, workspace file access
internal/links         the result model: link, status, reason, report
internal/checks        the checks
internal/mcpserver     MCP tools, HTTP mode, API keys
internal/cli           the command line
internal/tools         the licence collector used by the image build
tests                  end-to-end tests over MCP stdio and the CLI
```

The CLI and the MCP server are thin layers over `internal/checks`. Neither
has checking logic of its own.

The checks are listed in one table, `registry` in
`internal/checks/registry.go`. A second table, `runners`, says which
function produces which checks. The dispatch, the `checks` command, the
`list_checks` tool and the check selection all read from these tables.

## How a check runs

1. Every document is parsed into a plain map. Lists are flattened.
2. Resources are indexed by API group, kind, namespace and name. Using the
   group keeps a custom `Service` kind apart from the built-in one.
3. Pod templates are collected from every resource that has one.
4. Each resource in the bundle is checked. Cluster objects are indexed but
   not checked.
5. Every reference that is followed becomes a link with a status. Links of
   a check that is turned off are dropped at this point.
6. The verdict is worked out from all the links.

Manifests are read as plain maps, not as typed Kubernetes objects. A field
with the wrong type is then simply not followed, and the other checks still
run. Reporting that field is the schema validator's job.

Label selectors are the exception. They are converted to the Kubernetes
types and matched with `k8s.io/apimachinery/pkg/labels`, so `matchLabels`
and `matchExpressions` behave exactly as they do in a cluster.

## Failure handling

- Input that cannot be parsed is an error for the whole request.
- In a directory, a file that is not a manifest is listed under
  `skippedFiles` and the rest is checked.
- A check that panics becomes an `error` link for that resource. The report
  is then not complete.
- A check that runs out of time returns an error, not a partial result.

## Secrets

The bundle often holds Secret values. The checks read the key names of a
Secret and nothing else. No value is copied into a link, a message or an
explanation. Tests check this at the unit level and over MCP.

## Libraries

| Library | Used for |
| --- | --- |
| `github.com/modelcontextprotocol/go-sdk` | MCP over stdio and HTTP |
| `k8s.io/apimachinery` | YAML decoding and label selectors |
| `github.com/spf13/cobra` | The command line |
| `golang.org/x/time` | Rate limits in hosted mode |

There is no Kubernetes client in the program.
