# Hosting

One copy of the server can serve many agents over HTTP. Each caller needs an
API key, and each key has its own rate limit.

```bash
kube-refs-mcp serve --http 127.0.0.1:8089 --keys /etc/kube-refs-mcp/keys
```

MCP is served at `/mcp`. `/healthz` is open and needs no key.

Serve it over HTTPS only, because the key travels in a header on every
request. Listen on `127.0.0.1` and put a reverse proxy with TLS in front.

Keep in mind what hosting means: callers' manifests are sent to your server.
They are held in memory, never written or logged, but they do pass through
it and any proxy in front of it. Manifests can contain secrets. For anything
sensitive, run the server locally over stdio instead.

## API keys

Make a key for each person or team:

```bash
sudo sh -c 'kube-refs-mcp new-key alice >> /etc/kube-refs-mcp/keys'
```

The key is printed once, for you to pass on. The line added to the keys file
holds the name and the SHA-256 of the key, never the key itself. A lost key
cannot be recovered: make a new one.

```text
# /etc/kube-refs-mcp/keys
alice 5f2b…e91c
ci-pipeline 9a40…77d2
```

To revoke a key, delete its line. The server re-reads the file within a few
seconds, with no restart. If an edit leaves the file broken, the server keeps
the keys it had and logs why.

## Rate limits

Each key may make 60 requests a minute, with a burst of 30. Change this with
`--rate` and `--burst`. One key's use does not affect another's.

| Response | Meaning |
| --- | --- |
| `401` | No key, or a key that is not in the file. |
| `429` | Over the limit. `Retry-After` says how long to wait. |

## Connecting an agent

```bash
claude mcp add --transport http kube-refs https://example.com/mcp --header "Authorization: Bearer <key>"
```

Or in `.mcp.json`:

```json
{
  "mcpServers": {
    "kube-refs": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer <key>"}
    }
  }
}
```

## Turning checks off for everyone

`serve --skip-check NAME` turns a check off for every caller. Callers see
it under `skippedByServer` in `list_checks`, and under `checksSkipped` in
every report. They cannot turn it back on.

## What a hosted server exposes

- Callers send manifests inline. The `files` and `directory` inputs read
  from the server's own `--workspace-root`. The set-ups below give it none,
  so those inputs are refused and nobody can read files from the server.
- Each key holder sees only their own checks. `explain_finding` cannot read
  a finding from someone else's check.
- The log has one line per request: the key's name, the status and the time
  taken. The request body is never logged.
- Every call stands alone, so the server can sit behind a proxy or a load
  balancer. The history that `explain_finding` uses is kept in memory per
  process. With several copies, an explanation may need the check to be run
  again.

## Set up with systemd

The files in [deploy](../deploy) run the server as a systemd service behind
Apache. It needs one file, the API keys.

```sh
CGO_ENABLED=0 go build -trimpath -o kube-refs-mcp ./cmd/kube-refs-mcp   # on any machine
sudo install -m 755 kube-refs-mcp /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin kube-refs-mcp
sudo install -d -m 750 -o root -g kube-refs-mcp /etc/kube-refs-mcp
sudo install -m 640 -o root -g kube-refs-mcp /dev/null /etc/kube-refs-mcp/keys
sudo install -m 644 deploy/kube-refs-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kube-refs-mcp
curl http://127.0.0.1:8089/healthz                  # prints ok
```

Then route `/mcp` to it from Apache. Copy `deploy/kube-refs-mcp-apache.conf`
to `/etc/apache2/` and include it in the site's HTTPS `<VirtualHost>`:

```sh
sudo a2enmod proxy proxy_http
sudo apachectl configtest && sudo systemctl reload apache2
```

## Set up with Docker

The image is `ghcr.io/cosmosdevops/kube-refs-mcp`, built from every commit
on `main`. It holds the program and the licences of the code built into it,
and nothing else. It runs as an unprivileged user (ID 65532).

Make the keys directory readable by that user, then start the compose file
from `deploy/`:

```sh
sudo install -d -m 750 -o root -g 65532 /etc/kube-refs-mcp
sudo install -m 640 -o root -g 65532 /dev/null /etc/kube-refs-mcp/keys
docker compose -f deploy/compose.yaml up -d
curl http://127.0.0.1:8089/healthz                  # prints ok
```

The compose file mounts the keys directory read-only, runs the container
with a read-only file system and no capabilities, and publishes the port on
localhost only. Route `/mcp` to it from Apache as above.

Make keys with the image too:

```sh
sudo sh -c 'docker run --rm ghcr.io/cosmosdevops/kube-refs-mcp new-key alice >> /etc/kube-refs-mcp/keys'
```

To update, pull and start again. Only the short history used by
`explain_finding` is lost.

```sh
docker compose -f deploy/compose.yaml pull && docker compose -f deploy/compose.yaml up -d
```

The container has no shell or curl, so its health check uses the program
itself: `kube-refs-mcp check-health http://127.0.0.1:8089/healthz` exits 0
when the server is up.

### Building the image

```sh
docker build --build-arg VERSION=1.2.3 -t kube-refs-mcp:local .
```

### Podman and SELinux

The image and the compose file should run unchanged with `podman` and
`podman-compose`.

- As root (`sudo podman …`), set up the keys directory exactly as above.
- Rootless, user IDs inside the container are shifted, so ID 65532 cannot
  read a file that only your own user can. Map the container's user onto
  yours with `--userns=keep-id:uid=65532,gid=65532` (in compose:
  `userns_mode: "keep-id:uid=65532,gid=65532"`), and have your own user own
  the keys directory.

On a host with SELinux (Fedora, RHEL), add `,z` to the volume line, as in
`/etc/kube-refs-mcp:/config:ro,z`.

### Using the image locally

The image also works over stdio, with no Go needed. With no volume, the
agent sends manifests inline:

```json
{
  "mcpServers": {
    "kube-refs": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "ghcr.io/cosmosdevops/kube-refs-mcp"]
    }
  }
}
```

To let it read files, mount the project and name it as a workspace root:

```json
{
  "mcpServers": {
    "kube-refs": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "-v", "/path/to/project:/workspace:ro",
               "ghcr.io/cosmosdevops/kube-refs-mcp", "serve", "--workspace-root", "/workspace"]
    }
  }
}
```

## Releasing

The version is the git tag. Nothing in the code needs changing:

```sh
git tag -a v1.2.3 -m "kube-refs-mcp 1.2.3"
git push origin v1.2.3
```

CI then publishes the `1.2.3` image, and `go install …@v1.2.3` works.
`kube-refs-mcp version` reports the tag in both. A build between tags
reports a version made from the last tag and the commit, and a build with no
git history to read reports `dev`.
