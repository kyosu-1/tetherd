package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/version"
)

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return string(out), nil
}

func main() {
	var (
		socket     = flag.String("socket", helper.DefaultSocket, "UNIX socket path")
		execSrc    = flag.String("exec-src", "", "path of the tetherd-exec binary to install (default: next to this binary)")
		installDir = flag.String("install-dir", helper.ExecInstallDir, "where to place the setgid tetherd-exec")
		resolver   = flag.String("resolver-dir", "/etc/resolver", "resolver directory")
		showVer    = flag.Bool("version", false, "print version")
	)
	flag.Parse()
	if *showVer || (flag.NArg() > 0 && flag.Arg(0) == "version") {
		fmt.Println("tetherd-helper", version.Version)
		return
	}
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("tetherd-helper ")
	if runtime.GOOS != "darwin" {
		log.Fatal("tetherd-helper only runs on macOS")
	}
	if os.Geteuid() != 0 {
		log.Fatal("must run as root (sudo tetherd-helper, or via launchd)")
	}

	gid, err := helper.EnsureGroup(runCmd, helper.GroupName)
	if err != nil {
		log.Fatalf("group %s: %v", helper.GroupName, err)
	}
	src := *execSrc
	if src == "" {
		self, _ := os.Executable()
		src = filepath.Join(filepath.Dir(self), helper.ExecName)
	}
	execPath, err := helper.InstallExec(src, *installDir, gid)
	if err != nil {
		log.Fatalf("install %s: %v", helper.ExecName, err)
	}
	log.Printf("group %s gid=%d, %s installed at %s", helper.GroupName, gid, helper.ExecName, execPath)

	platform := newPlatform(*resolver, log.Printf)
	if err := platform.Shutdown(); err != nil { // clear leftovers from a crashed run
		log.Printf("startup cleanup: %v", err)
	}
	srv := &helper.Server{Platform: platform, Allow: helper.AllowAdmin, Logf: log.Printf}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("listening on %s", *socket)
	err = srv.ListenAndServe(ctx, *socket)
	if serr := platform.Shutdown(); serr != nil {
		log.Printf("shutdown: %v", serr)
	}
	if err != nil {
		log.Fatalf("serve: %v", err)
	}
}
