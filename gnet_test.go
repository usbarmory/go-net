// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package gnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/soypat/lneto/arp"
	"github.com/soypat/lneto/ethernet"
)

const arpRequest = `ffffffffffffaabbccddeeff08060001080006040001aabbccddeeff0a0000010000000000000a000003`

type DummyNIC struct {
	buf []byte
}

func (d *DummyNIC) Receive(buf []byte) (n int, err error) {
	return
}

func (d *DummyNIC) Transmit(buf []byte) (err error) {
	d.buf = buf
	fmt.Printf("tx (%d bytes): %x\n", len(buf), buf)
	return
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
	ip := netip.MustParsePrefix(addr)
	// build expected Ethernet+ARP frame.
	var buf [maxFrameLen]byte
	efrm, _ := ethernet.NewFrame(buf[:])
	*efrm.DestinationHardwareAddr() = ethernet.BroadcastAddr()
	*efrm.SourceHardwareAddr() = [6]byte(hwaddr)
	efrm.SetEtherType(ethernet.TypeARP)
	afrm, _ := arp.NewFrame(efrm.Payload())
	senderHW, senderIP := afrm.Sender4()
	targetHW, targetIP := afrm.Target4()
	*senderHW = [6]byte(hwaddr)
	*targetHW = ethernet.BroadcastAddr()
	*senderIP = ip.Addr().As4()
	*targetIP = [4]byte{}
	afrm.SetHardware(1, 6)
	afrm.SetOperation(arp.OpRequest)
	afrm.SetProtocol(ethernet.TypeIPv4, 4)

	payload, err := hex.DecodeString(arpRequest)

	if err != nil {
		t.Fatal(err)
	}

	iface := &Interface{
		Stack: stack,
	}

	nic := &DummyNIC{}

	if err := iface.Init(addr, mac, gateway); err != nil {
		panic(err)
	}

	raddr := &net.TCPAddr{
		IP:   net.ParseIP(remoteAddr),
		Port: remotePort,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go iface.Start(ctx)

	_, err = iface.Stack.Socket(ctx, "tcp", syscall.AF_INET, syscall.SOCK_STREAM, nil, raddr)
	if ctx.Err() != nil {
		t.Fatal("unexpected blocking in stack:", ctx.Err())
	} else if err != nil {
		t.Log("expect no route:", err) // Should return error since no answer received.
	}
	if !bytes.Equal(nic.buf, payload) {
		t.Errorf("tx payload mismatch:\n  %x\n  %x", nic.buf, payload)
	}

	if err.Error() != "connect tcp 10.0.0.3:80: no route to host" {
		t.Errorf("unexpected error, %v", err.Error())
	}
}
