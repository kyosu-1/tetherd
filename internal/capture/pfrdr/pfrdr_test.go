package pfrdr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/helper"
)

type fakeHelper struct {
	applied *helper.PfSpec
	cleared bool
	lookups []string

	// failNatLookN, if > 0, makes the first N NatLook calls fail.
	failNatLookN int
	natLookCalls int
}

func (f *fakeHelper) PfApply(s helper.PfSpec) error { f.applied = &s; return nil }
func (f *fakeHelper) PfClear() error                { f.cleared = true; return nil }
func (f *fakeHelper) NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error) {
	f.lookups = append(f.lookups, proto+" "+src.String()+" -> "+dst.String())
	f.natLookCalls++
	if f.natLookCalls <= f.failNatLookN {
		return netip.AddrPort{}, errors.New("natlook: fake failure")
	}
	return netip.MustParseAddrPort("10.0.3.21:5432"), nil
}

func TestStartAppliesRulesWithListenPort(t *testing.T) {
	fh := &fakeHelper{}
	c := New(fh)
	spec := capture.Spec{RemoteCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}
	if err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if fh.applied == nil || fh.applied.RedirectPort != c.RedirectPort() || c.RedirectPort() == 0 {
		t.Fatalf("applied = %+v, port = %d", fh.applied, c.RedirectPort())
	}
	if len(fh.applied.RemoteCIDRs) != 1 || fh.applied.RemoteCIDRs[0].String() != "10.0.0.0/16" {
		t.Fatalf("cidrs = %v", fh.applied.RemoteCIDRs)
	}
}

func TestAcceptResolvesOriginalDst(t *testing.T) {
	fh := &fakeHelper{}
	c := New(fh)
	if err := c.Start(context.Background(), capture.Spec{RemoteCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go func() {
		// Simulate a redirected connection arriving on the transparent port.
		conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.RedirectPort())))
		if err == nil {
			time.Sleep(100 * time.Millisecond)
			conn.Close()
		}
	}()
	cc, err := c.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if cc.OriginalDst != netip.MustParseAddrPort("10.0.3.21:5432") {
		t.Fatalf("OriginalDst = %v", cc.OriginalDst)
	}
	if len(fh.lookups) != 1 || fh.lookups[0][:4] != "tcp " {
		t.Fatalf("lookups = %v", fh.lookups)
	}
}

func TestCloseClearsRules(t *testing.T) {
	fh := &fakeHelper{}
	c := New(fh)
	if err := c.Start(context.Background(), capture.Spec{RemoteCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !fh.cleared {
		t.Fatal("Close must call PfClear")
	}
	if _, err := c.Accept(); err == nil {
		t.Fatal("Accept after Close must fail")
	}
}

// TestAcceptSkipsNatLookFailureAndContinues verifies that a natlook failure
// on one connection does not fail Accept: the failed connection is closed
// and Accept keeps looping until a connection resolves (or the listener
// itself errors).
func TestAcceptSkipsNatLookFailureAndContinues(t *testing.T) {
	fh := &fakeHelper{failNatLookN: 1}
	c := New(fh)
	var logs []string
	c.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if err := c.Start(context.Background(), capture.Spec{RemoteCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.RedirectPort()))
	conn1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	cc, err := c.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if cc.OriginalDst != netip.MustParseAddrPort("10.0.3.21:5432") {
		t.Fatalf("OriginalDst = %v", cc.OriginalDst)
	}
	if fh.natLookCalls != 2 {
		t.Fatalf("natLookCalls = %d, want 2 (one failed, one succeeded)", fh.natLookCalls)
	}

	// The first (failed) connection's peer should observe EOF, because
	// Accept closed the server side of it before moving on.
	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn1.Read(buf); err != io.EOF {
		t.Fatalf("conn1.Read = %v, want io.EOF", err)
	}

	if len(logs) != 1 {
		t.Fatalf("logs = %v, want exactly one entry for the failed natlook", logs)
	}
}
