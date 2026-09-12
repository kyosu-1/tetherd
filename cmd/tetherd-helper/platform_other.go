//go:build !darwin

package main

import (
	"log"

	"github.com/kyosu-1/tetherd/internal/helper"
)

type platform interface {
	helper.Platform
	Shutdown() error
	ClearLeftovers() error
}

func newPlatform(string, func(string, ...any)) platform {
	log.Fatal("tetherd-helper only runs on macOS")
	return nil
}
