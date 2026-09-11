package direct

import (
	"context"
	"net"
	"testing"

	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestDialConnectsToAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	go func() { c, _ := ln.Accept(); c.Close(); close(accepted) }()

	var tr transport.Transport = Transport{}
	c, err := tr.Dial(context.Background(), transport.Task{Addr: ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	<-accepted
}

func TestDialRequiresAddr(t *testing.T) {
	if _, err := (Transport{}).Dial(context.Background(), transport.Task{}); err == nil {
		t.Fatal("empty Addr must fail")
	}
}
