package main

import (
	"fmt"
	"os"

	"github.com/kyosu-1/tetherd/internal/cli"
)

func main() {
	root := cli.NewRootCommand()
	if err := root.Execute(); err != nil {
		if !cli.IsChildExit(err) {
			fmt.Fprintf(os.Stderr, "tetherd  ✗ %v\n", err)
		}
		os.Exit(cli.ExitCode(err))
	}
}
