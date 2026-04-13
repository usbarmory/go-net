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
	"syscall"
	"testing"
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

func TestGVisorStack(t *testing.T) {
	const (
		addr    = "10.0.0.1/24"
		gateway = "10.0.0.2"
		mac     = "aa:bb:cc:dd:ee:ff"

		remoteAddr = "10.0.0.3"
		remotePort = 80
	)

	payload, err := hex.DecodeString(arpRequest)

	if err != nil {
		t.Fatal(err)
	}

	iface := &Interface{
		Stack: NewGVisorStack(1),
	}

	nic := &DummyNIC{}

	if err := iface.Init(nic, addr, mac, gateway); err != nil {
		panic(err)
	}

	raddr := &net.TCPAddr{
		IP:   net.ParseIP(remoteAddr),
		Port: remotePort,
	}

	_, err = iface.Stack.Socket(context.Background(), "tcp", syscall.AF_INET, syscall.SOCK_STREAM, nil, raddr)

	if !bytes.Equal(nic.buf, payload) {
		t.Errorf("tx payload mismatch:\n  %x\n  %x", nic.buf, payload)
	}

	if err.Error() != "connect tcp 10.0.0.3:80: no route to host" {
		t.Errorf("unexpected error, %v", err.Error())
	}
}
