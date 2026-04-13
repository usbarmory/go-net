// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package gnet implements TCP/IP connectivity through a generic
// [NetworkDevice] interface and TCP/IP [Stack].
//
// The package provides [GVisorStack] as pure Go [Stack] implementation using
// gVisor.
//
// This package is designed for, but not limited to, use with `GOOS=tamago` as
// supported by the TamaGo framework for bare metal Go, see
// https://github.com/usbarmory/tamago.
package gnet

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"runtime"
)

const (
	// MTU represents the Ethernet Maximum Transmission Unit.
	MTU    = 1518
	hdrLen = 14
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

// Interface bridges a [Stack] and a [NetworkDevice], driving packet I/O
// between them.
type Interface struct {
	// Stack represents a [Stack] instance.
	Stack Stack
	// HandleStackErr defines an optional function to handle [Stack]
	// errors.
	HandleStackErr func(err error, tx bool)

	nic NetworkDevice
}

// Init initializes the interface with a [NetworkDevice], bridging it with a
// [Stack].
//
// If [Stack] is not set a default implementation is initialized, the stack is
// configured with the CIDR address and hardware (MAC) address.
//
// The gateway may or may not be provided, if MAC is empty it will be set to a
// random address.
func (iface *Interface) Init(nic NetworkDevice, addr string, mac string, gateway string) (err error) {
	var laddr net.HardwareAddr

	if nic == nil {
		return errors.New("invalid nic")
	}

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

	if iface.Stack == nil {
		iface.Stack = NewGVisorStack(1)
	}

	gwaddr, _ := netip.ParseAddr(gateway)

	if err = iface.Stack.Configure(laddr.String(), pfx, gwaddr); err != nil {
		return err
	}

	iface.Stack.SetWriteNotify(iface.notifyTx)
	iface.nic = nic

	return
}

// Start begins processing of incoming packets, the function receives packets
// through [NetworkDevice.Receive] and handles them through
// [Stack.RecvInboundPacket], it should never return.
func (iface *Interface) Start() {
	buf := make([]byte, MTU)

	for {
		n, err := iface.rx(buf)

		if err != nil || n == 0 {
			runtime.Gosched()
		}
	}
}

func (iface *Interface) tx(buf []byte) (n int, err error) {
	if n, err = iface.Stack.WriteOutboundPacket(buf); err != nil {
		if iface.HandleStackErr != nil {
			iface.HandleStackErr(err, true)
		}

		return
	}

	if n < hdrLen {
		return 0, nil
	}

	return n, iface.nic.Transmit(buf[:n])
}

func (iface *Interface) rx(buf []byte) (n int, err error) {
	n, err = iface.nic.Receive(buf)

	if n == 0 || err != nil {
		return
	}

	if err = iface.Stack.RecvInboundPacket(buf[:n]); err != nil {
		if iface.HandleStackErr != nil {
			iface.HandleStackErr(err, false)
		}
	}

	return
}

func (iface *Interface) notifyTx() {
	iface.tx(make([]byte, MTU+18)) // FIXME: why +18 ?
}
