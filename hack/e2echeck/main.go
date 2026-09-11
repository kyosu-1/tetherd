// e2echeck fetches a URL and prints the body; used by hack/e2e-local.sh to
// verify capture from a Go child process.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
}
