package main

import (
	"fmt"
	"os"

	"github.com/badriram/bonsai/internal/cli"
)

// version is set at build time by the Makefile via -ldflags. Defaults to "dev"
// for `go run` and bare `go build` invocations.
var version = "dev"

func main() {
	root := cli.NewRootCommand()
	root.Version = version
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
