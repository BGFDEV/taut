// Command taut migrates Slack workspaces to Microsoft Teams.
package main

import (
	"context"
	"os"

	"github.com/bgfdev/taut/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
}
