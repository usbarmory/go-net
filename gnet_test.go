// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package gnet

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/soypat/lneto/arp"
	"github.com/soypat/lneto/ethernet"
)

type DummyNIC struct {
	mu  sync.Mutex
	txd [][]byte
}

func (d *DummyNIC) Receive(buf []byte) (n int, err error) {
	return
}

func (d *DummyNIC) Transmit(buf []byte) error {
	d.mu.Lock()
	d.txd = append(d.txd, append([]byte(nil), buf...))
	d.mu.Unlock()
	fmt.Printf("tx (%d bytes): %x\n", len(buf), buf)
	return nil
}

func (d *DummyNIC) HasFrame(want []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.txd {
		if frameMatches(p, want) {
			return true
		}
	}
	return false
}

// frameMatches reports whether got equals want, allowing trailing zero
// bytes that some stacks pad to reach the 60-byte Ethernet minimum.
func frameMatches(got, want []byte) bool {
	if !bytes.HasPrefix(got, want) {
		return false
	}
	for _, b := range got[len(want):] {
		if b != 0 {
			return false
		}
	}
	return true
}

func (d *DummyNIC) Frames() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, len(d.txd))
	copy(out, d.txd)
	return out
}

func TestStacks(t *testing.T) {
	t.Run("arp gVisor", func(t *testing.T) {
		start := time.Now()
		testStackARP(t, NewGVisorStack(1))
		elapsed := time.Since(start)
		t.Log(t.Name(), "elapsed", elapsed)
	})
	t.Run("arp lneto", func(t *testing.T) {
		start := time.Now()
		testStackARP(t, NewLnetoStack("lneto", nil))
		elapsed := time.Since(start)
		t.Log(t.Name(), "elapsed", elapsed)
	})
}

func testStackARP(t *testing.T, stack Stack) {
	const (
		addr    = "10.0.0.1/24"
		gateway = "10.0.0.2"
		mac     = "aa:bb:cc:dd:ee:ff"

		remoteAddr  = "10.0.0.3"
		remotePort  = 80
		maxFrameLen = 1518
	)
	hwaddr, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParsePrefix(addr)
	rip := netip.MustParseAddr(remoteAddr)

	// Build the expected Ethernet+ARP request frame the stack should emit
	// when resolving remoteAddr (which sits in the local /24 subnet).
	var expectBuf [maxFrameLen]byte
	efrm, err := ethernet.NewFrame(expectBuf[:])
	if err != nil {
		t.Fatal(err)
	}
	*efrm.DestinationHardwareAddr() = ethernet.BroadcastAddr()
	*efrm.SourceHardwareAddr() = [6]byte(hwaddr)
	efrm.SetEtherType(ethernet.TypeARP)
	afrm, err := arp.NewFrame(efrm.Payload())
	if err != nil {
		t.Fatal(err)
	}
	afrm.SetHardware(1, 6)
	afrm.SetProtocol(ethernet.TypeIPv4, 4)
	afrm.SetOperation(arp.OpRequest)
	senderHW, senderIP := afrm.Sender4()
	targetHW, targetIP := afrm.Target4()
	*senderHW = [6]byte(hwaddr)
	*targetHW = [6]byte{} // unknown — to be filled in by ARP reply.
	*senderIP = ip.Addr().As4()
	*targetIP = rip.As4()
	const ethHdrLen, arpV4Len = 14, 28
	expected := expectBuf[:ethHdrLen+arpV4Len]

	nic := &DummyNIC{}
	iface := &Interface{
		Stack:         stack,
		NetworkDevice: nic,
	}

	if err := iface.Init(addr, mac, gateway); err != nil {
		t.Fatal(err)
	}

	raddr := &net.TCPAddr{
		IP:   net.ParseIP(remoteAddr),
		Port: remotePort,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	go iface.Start(ctx)

	// Drain the stack's egress queue into the NIC. The gVisor channel.Endpoint
	// fires WriteNotify on its own, but LnetoStack's writenotify only fires on
	// rx and after Socket returns — so we poll WriteOutboundPacket to flush
	// packets queued during a blocking Socket call.
	go func() {
		buf := make([]byte, MTU+EthernetMaximumSize)
		for ctx.Err() == nil {
			n, _ := iface.Stack.WriteOutboundPacket(buf)
			if n < EthernetMinimumSize {
				time.Sleep(time.Millisecond)
				continue
			}
			nic.Transmit(buf[:n])
		}
	}()

	_, err = iface.Stack.Socket(ctx, "tcp", syscall.AF_INET, syscall.SOCK_STREAM, nil, raddr)
	if err == nil {
		t.Error("expected Socket to fail (no peer to answer ARP); got nil error")
	} else {
		t.Log("Socket returned (expected):", err)
	}

	// Give the polling goroutine a moment to drain any final queued frames.
	time.Sleep(10 * time.Millisecond)

	if !nic.HasFrame(expected) {
		t.Errorf("expected ARP request not transmitted\n  want: %x", expected)
		for i, f := range nic.Frames() {
			t.Logf("  tx[%d]: %x", i, f)
		}
	}
}
