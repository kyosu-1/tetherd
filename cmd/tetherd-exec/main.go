// tetherd-exec is installed setgid "tetherd" by tetherd-helper. It makes the
// real and effective gid both "tetherd" (so bash does not drop it) and execs
// the command. It grants nothing else: the gid only makes pf capture the
// process's sockets.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/kyosu-1/tetherd/internal/version"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "version" {
		fmt.Println("tetherd-exec", version.Version)
		return
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tetherd-exec -- <command> [args...]")
		os.Exit(2)
	}
	egid := os.Getegid()
	if egid == os.Getgid() {
		fmt.Fprintln(os.Stderr, "tetherd-exec: not running setgid; run `tetherd doctor` (is tetherd-helper installed?)")
		os.Exit(3)
	}
	if err := syscall.Setregid(egid, egid); err != nil {
		fmt.Fprintln(os.Stderr, "tetherd-exec: setregid:", err)
		os.Exit(3)
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "tetherd-exec:", err)
		os.Exit(127)
	}
	if err := syscall.Exec(path, args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "tetherd-exec: exec:", err)
		os.Exit(126)
	}
}
