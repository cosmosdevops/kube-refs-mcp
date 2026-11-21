// Command kube-refs-mcp checks that a bundle of Kubernetes manifests fits
// together, as an MCP server or from the command line.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/cosmosdevops/kube-refs-mcp/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
