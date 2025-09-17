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
	"crypto/rand"
	"fmt"
	"net"
	"strconv"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

var (
	// MTU represents the Ethernet Maximum Transmission Unit.
	MTU uint32 = 1518

	// NICID represents the default gVisor NIC identifier
	NICID = tcpip.NICID(1)

	// DefaultStackOptions represents the default gVisor Stack configuration
	DefaultStackOptions = stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			icmp.NewProtocol4,
			udp.NewProtocol},
	}
)

// Interface represents an Ethernet interface instance.
type Interface struct {
	NICID tcpip.NICID
	NIC   *NIC

	Stack *stack.Stack
	Link  *channel.Endpoint
}

func (iface *Interface) configure(mac string, ip tcpip.AddressWithPrefix, gw tcpip.Address) (err error) {
	if iface.Stack == nil {
		iface.Stack = stack.New(DefaultStackOptions)
	}

	linkAddr, err := tcpip.ParseMACAddress(mac)

	if err != nil {
		return
	}

	iface.Link = channel.New(256, MTU, linkAddr)
	iface.Link.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	linkEP := stack.LinkEndpoint(iface.Link)

	if err := iface.Stack.CreateNIC(iface.NICID, linkEP); err != nil {
		return fmt.Errorf("%v", err)
	}

	protocolAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ip,
	}

	if err := iface.Stack.AddProtocolAddress(iface.NICID, protocolAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("%v", err)
	}

	rt := iface.Stack.GetRouteTable()

	rt = append(rt, tcpip.Route{
		Destination: protocolAddr.AddressWithPrefix.Subnet(),
		NIC:         iface.NICID,
	})

	rt = append(rt, tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		Gateway:     gw,
		NIC:         iface.NICID,
	})

	iface.Stack.SetRouteTable(rt)

	return
}

// EnableICMP adds an ICMP endpoint to the interface, it is useful to enable
// ping requests.
func (iface *Interface) EnableICMP() error {
	var wq waiter.Queue

	ep, err := iface.Stack.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)

	if err != nil {
		return fmt.Errorf("endpoint error (icmp): %v", err)
	}

	addr, tcpErr := iface.Stack.GetMainNICAddress(iface.NICID, ipv4.ProtocolNumber)

	if tcpErr != nil {
		return fmt.Errorf("couldn't get NIC IP address: %v", tcpErr)
	}

	fullAddr := tcpip.FullAddress{Addr: addr.Address, Port: 0, NIC: iface.NICID}

	if err := ep.Bind(fullAddr); err != nil {
		return fmt.Errorf("bind error (icmp endpoint): ", err)
	}

	return nil
}

// fullAddr attempts to convert the ip:port to a FullAddress struct.
func fullAddr(a string) (tcpip.FullAddress, error) {
	var p int

	host, port, err := net.SplitHostPort(a)

	if err == nil {
		if p, err = strconv.Atoi(port); err != nil {
			return tcpip.FullAddress{}, err
		}
	} else {
		host = a
	}

	addr := net.ParseIP(host)
	return tcpip.FullAddress{Addr: tcpip.AddrFromSlice(addr.To4()), Port: uint16(p)}, nil
}

// Init initializes a [NetworkDevice] associating it to a gVisor link, a
// default NICID and TCP/IP gVisor Stack are set if not previously assigned, a
// random MAC address is set if its argument is empty.
func (iface *Interface) Init(nic NetworkDevice, addr string, mac string, gateway string) (err error) {
	var laddr net.HardwareAddr

	ip, ipnet, err := net.ParseCIDR(addr)

	if err != nil {
		return
	}

	if len(mac) == 0 {
		laddr = make([]byte, 6)
		rand.Read(laddr)
		// flag address as unicast and locally administered
		laddr[0] &= 0xfe
		laddr[0] |= 0x02
	} else {
		if laddr, err = net.ParseMAC(mac); err != nil {
			return
		}
	}

	if iface.NICID == 0 {
		iface.NICID = NICID
	}

	ipAddr := tcpip.AddressWithPrefix{
		Address:   tcpip.AddrFromSlice(ip.To4()),
		PrefixLen: tcpip.MaskFromBytes(ipnet.Mask).Prefix(),
	}

	gwAddr := tcpip.AddrFromSlice(net.ParseIP(gateway)).To4()

	if err = iface.configure(laddr.String(), ipAddr, gwAddr); err != nil {
		return
	}

	if iface.NIC == nil {
		iface.NIC = &NIC{
			MAC:    laddr,
			Link:   iface.Link,
			Device: nic,
		}

		err = iface.NIC.Init()
	}

	return
}
