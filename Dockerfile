# kube-refs-mcp as a small image: one static binary and the licences of
# the open source code built into it. Nothing else, not even a shell.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# The build has no git history to read the version from, so it is passed in:
#   docker build --build-arg VERSION=1.2.3 .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/cosmosdevops/kube-refs-mcp/internal/app.buildVersion=${VERSION}" \
    -o /kube-refs-mcp ./cmd/kube-refs-mcp
# Open source licences ask that their text travels with the binary. Collect
# the licence and notice files of exactly the modules that were linked in.
COPY LICENSE NOTICE ./
RUN go run ./internal/tools/licenses -binary /kube-refs-mcp -out /licenses

FROM scratch
COPY --from=build /kube-refs-mcp /kube-refs-mcp
COPY --from=build /licenses /licenses
# Runs as an unprivileged user. Files it must read (the API keys, and any
# workspace that is mounted in) need to be readable by this ID; see
# deploy/compose.yaml.
USER 65532:65532
EXPOSE 8089
ENTRYPOINT ["/kube-refs-mcp"]
# With no arguments it talks MCP over stdin/stdout, as "serve" does:
#   docker run --rm -i ghcr.io/cosmosdevops/kube-refs-mcp
CMD ["serve"]
