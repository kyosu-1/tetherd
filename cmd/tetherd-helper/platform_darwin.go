//go:build darwin

package main

import (
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/helper/pf"
)

type platform interface {
	helper.Platform
	Shutdown() error
}

func newPlatform(resolverDir string, logf func(string, ...any)) platform {
	return helper.NewDarwinPlatform(pf.ExecRunner, resolverDir, logf)
}
