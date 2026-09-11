package main

import (
	"fmt"
	"os"

	"github.com/kyosu-1/tetherd/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("tetherd-exec", version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "tetherd-exec: not implemented yet")
	os.Exit(2)
}
