BINARY := bin/kube-refs-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/cosmosdevops/kube-refs-mcp/internal/app.buildVersion=$(VERSION)

.PHONY: build image licenses test vet

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/kube-refs-mcp

# Container image; see deploy/compose.yaml.
image:
	docker build --build-arg VERSION=$(VERSION) -t kube-refs-mcp:local .

# Licence and notice files of every module built into the binary. The
# container image has the same set at /licenses.
licenses: build
	go run ./internal/tools/licenses -binary $(BINARY) -out bin/licenses

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal tests)" || (echo "gofmt needed:"; gofmt -l cmd internal tests; exit 1)
