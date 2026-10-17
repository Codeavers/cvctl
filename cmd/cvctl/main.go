package main

import (
	"os"

	"github.com/codeavers/cvctl/internal/cli"
)

// Overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cli.Execute(cli.NewRoot(version), os.Args[1:], os.Stdout, os.Stderr))
}
