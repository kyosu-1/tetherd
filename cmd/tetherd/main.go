package main

import (
	"fmt"
	"os"

	"github.com/kyosu-1/tetherd/internal/cli"
)

func main() {
	root := cli.NewRootCommand()
	if err := root.Execute(); err != nil {
		code := cli.ExitCode(err)
		if code == 1 || code == 2 {
			fmt.Fprintf(os.Stderr, "tetherd  ✗ %v\n", err)
		}
		os.Exit(code)
	}
}
