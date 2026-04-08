// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package gnet implements TCP/IP connectivity through a generic
// [NetworkDevice] interface.
//
// The TCP/IP stack is implemented using gVisor pure Go implementation.
//
// This package is only meant to be used with `GOOS=tamago` as
// supported by the TamaGo framework for bare metal Go, see
// https://github.com/usbarmory/tamago.
package gnet

import (
	"context"
	"crypto/rand"
	"net"
	"net/netip"
	"runtime"
)

var (
	// MTU represents the Ethernet Maximum Transmission Unit.
	MTU uint32 = 1518
)

// NetworkDevice represents a generic network device interface capable of
// receiving and transmitting raw Ethernet frames.
type NetworkDevice interface {
	// Receive receives a single Ethernet frame from a network adapter.
	Receive(buf []byte) (n int, err error)
	// Transmit transmits a single Ethernet frame to a network adapter.
	Transmit(buf []byte) (err error)
}

// Stack is the interface for a network stack implementation.
// It manages a single NIC and provides socket-level networking.
type Stack interface {
	// Configure sets the NIC ID, MAC address, IP prefix and gateway.
	// Gateway may be invalid.
	Configure(mac string, ip netip.Prefix, gw netip.Addr) error
	// HardwareAddress returns the MAC address of the NIC.
	HardwareAddress() (net.HardwareAddr, error)
	// EnableICMP registers an ICMP handler on the stack.
	EnableICMP() error
	// Socket creates a network socket bound to laddr and connected to raddr.
	Socket(ctx context.Context, network string, family, sotype int, laddr, raddr net.Addr) (c interface{}, err error)
	// SetWriteNotify registers a callback invoked when outbound data is ready.
	SetWriteNotify(cb func())
	// WriteOutboundPacket dequeues one outbound packet into buf, returning bytes written.
	WriteOutboundPacket(buf []byte) (int, error)
	// RecvInboundPacket delivers an inbound packet to the stack.
	RecvInboundPacket(buf []byte) error
}

// NewDefaultStack returns a ready-to-use [Stack] implementation as defined by
// build tags being used.
func NewDefaultStack() Stack {
	return newDefaultStack()
}

// Interface bridges a [Stack] and a [NetworkDevice], driving
// packet I/O between them. HandleStackErr, if non-nil, is called
// on stack errors during TX (tx=true) or RX (tx=false).
type Interface struct {
	netstack       Stack
	nic            NetworkDevice
	HandleStackErr func(err error, tx bool)
}

// NetworkingStack returns the [Stack] with which the interface was created with.
func (iface *Interface) NetworkingStack() Stack {
	return iface.netstack
}

// Init initializes the interface with a [NetworkDevice] and [Stack], bridging the two.
// The Stack is also configured with the CIDR address and hardware (MAC) address.
// Gateway may or may not be provided. If MAC is empty it will be set to a random address.
func (iface *Interface) Init(nic NetworkDevice, stack Stack, addr string, mac string, gateway string) (err error) {
	var laddr net.HardwareAddr
	pfx, err := netip.ParsePrefix(addr)
	if err != nil {
		return err
	}

	if len(mac) == 0 {
		laddr = make([]byte, 6)
		rand.Read(laddr)
		// flag address as unicast and locally administered
		laddr[0] &= 0xfe
		laddr[0] |= 0x02
	} else {
		if laddr, err = net.ParseMAC(mac); err != nil {
			return err
		}
	}

	gwaddr, _ := netip.ParseAddr(gateway)
	err = stack.Configure(laddr.String(), pfx, gwaddr)
	if err != nil {
		return err
	}
	stack.SetWriteNotify(iface.doTx)
	iface.nic = nic
	iface.netstack = stack
	return nil
}

// StartRx begins processing of incoming packets, the function receives packets
// through [NetworkDevice.Receive] and handles them through [NIC.Rx], it should
// never return.
func (iface *Interface) StartRx() {
	buf := make([]byte, MTU)
	for {
		n, err := iface.singleRx(buf)
		if err == nil && n > 0 {
			runtime.Gosched()
		}
	}
}

func (iface *Interface) singleTx(buf []byte) (int, error) {
	n, err := iface.netstack.WriteOutboundPacket(buf)
	if err != nil {
		if iface.HandleStackErr != nil {
			iface.HandleStackErr(err, true)
		}
		return 0, err
	} else if n < 14 {
		return 0, nil
	}
	err = iface.nic.Transmit(buf[:n])
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (iface *Interface) singleRx(buf []byte) (int, error) {
	n, err := iface.nic.Receive(buf)
	if err != nil {
		return 0, err
	}
	err = iface.netstack.RecvInboundPacket(buf[:n])
	if err != nil && iface.HandleStackErr != nil {
		iface.HandleStackErr(err, false)
	}
	return n, err
}

func (iface *Interface) doTx() {
	iface.singleTx(make([]byte, MTU+18))
}
