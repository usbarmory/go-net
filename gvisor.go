// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package gnet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
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

// GVisorStack implements [Stack] using the gvisor.dev/gvisor package.
type GVisorStack struct {
	Stack      *stack.Stack
	Link       *channel.Endpoint
	prevNotify *channel.NotificationHandle
	NICID      tcpip.NICID
}

// NewGVisorStack returns a gvisor stack ready to configure with the given [tcpip.NICID].
func NewGVisorStack(nicid tcpip.NICID) *GVisorStack {
	return &GVisorStack{
		NICID: nicid,
	}
}

// HardwareAddress implements [Stack.HardwareAddress].
func (g *GVisorStack) HardwareAddress() (net.HardwareAddr, error) {
	addr := g.Link.LinkAddress()
	return net.HardwareAddr(addr), nil
}

// Configure implements [Stack.Configure].
func (g *GVisorStack) Configure(mac string, ip netip.Prefix, gw netip.Addr) (err error) {
	linkAddr, err := tcpip.ParseMACAddress(mac)

	if err != nil {
		return
	}

	if g.NICID == 0 {
		g.NICID = tcpip.NICID(NICID)
	}

	if g.Stack == nil {
		g.Stack = stack.New(DefaultStackOptions)
	}

	g.Link = channel.New(256, MTU, linkAddr)
	g.Link.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	linkEP := stack.LinkEndpoint(g.Link)

	if err := g.Stack.CreateNIC(g.NICID, linkEP); err != nil {
		return fmt.Errorf("%v", err)
	}

	addr := tcpip.AddressWithPrefix{
		Address:   tcpip.AddrFromSlice(ip.Addr().AsSlice()),
		PrefixLen: ip.Bits(),
	}

	protocolAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: addr,
	}

	if err := g.Stack.AddProtocolAddress(g.NICID, protocolAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("%v", err)
	}

	rt := g.Stack.GetRouteTable()
	rt = append(rt, tcpip.Route{
		Destination: protocolAddr.AddressWithPrefix.Subnet(),
		NIC:         g.NICID,
	})

	var gwaddr tcpip.Address

	if gw.IsValid() {
		gwaddr = tcpip.AddrFromSlice(net.ParseIP(gw.String())).To4()
	}

	rt = append(rt, tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		Gateway:     gwaddr,
		NIC:         g.NICID,
	})

	g.Stack.SetRouteTable(rt)

	return nil
}

// EnableICMP implements [Stack].
func (g *GVisorStack) EnableICMP() error {
	var wq waiter.Queue

	ep, err := g.Stack.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)

	if err != nil {
		return fmt.Errorf("endpoint error (icmp): %v", err)
	}

	addr, tcpErr := g.Stack.GetMainNICAddress(g.NICID, ipv4.ProtocolNumber)

	if tcpErr != nil {
		return fmt.Errorf("couldn't get NIC IP address: %v", tcpErr)
	}

	fullAddr := tcpip.FullAddress{Addr: addr.Address, Port: 0, NIC: g.NICID}

	if err := ep.Bind(fullAddr); err != nil {
		return fmt.Errorf("bind error (icmp endpoint): %s", err)
	}

	return nil
}

// Socket implements [Stack.Socket].
func (g *GVisorStack) Socket(ctx context.Context, network string, family, sotype int, laddr, raddr net.Addr) (c interface{}, err error) {
	var proto tcpip.NetworkProtocolNumber
	var lFullAddr tcpip.FullAddress
	var rFullAddr tcpip.FullAddress

	if laddr != nil {
		if lFullAddr, err = gvisorFullAddr(laddr.String()); err != nil {
			return
		}
	}

	if raddr != nil {
		if rFullAddr, err = gvisorFullAddr(raddr.String()); err != nil {
			return
		}
	}

	switch family {
	case syscall.AF_INET:
		proto = ipv4.ProtocolNumber
	default:
		return nil, errors.New("unsupported address family")
	}

	switch network {
	case "udp", "udp4":
		if sotype != syscall.SOCK_DGRAM {
			return nil, errors.New("unsupported socket type")
		}

		if raddr != nil {
			c, err = gonet.DialUDP(g.Stack, &lFullAddr, &rFullAddr, proto)
		} else {
			c, err = gonet.DialUDP(g.Stack, &lFullAddr, nil, proto)
		}
	case "tcp", "tcp4":
		if sotype != syscall.SOCK_STREAM {
			return nil, errors.New("unsupported socket type")
		}

		if raddr != nil {
			c, err = gonet.DialContextTCP(ctx, g.Stack, rFullAddr, proto)
		} else {
			c, err = gonet.ListenTCP(g.Stack, lFullAddr, proto)
		}
	default:
		return nil, errors.New("unsupported network")
	}

	return c, err
}

type writeNotifierFunc func()

func (fn writeNotifierFunc) WriteNotify() {
	fn()
}

// SetWriteNotify implements [Stack.SetWriteNotify].
func (g *GVisorStack) SetWriteNotify(notifier func()) {
	if g.prevNotify != nil {
		g.Link.RemoveNotify(g.prevNotify)
	}
	g.prevNotify = g.Link.AddNotify(writeNotifierFunc(notifier))
}

// WriteOutboundPacket implements [Stack.WriteOutboundPacket].
func (g *GVisorStack) WriteOutboundPacket(buf []byte) (int, error) {
	var pkt *stack.PacketBuffer

	if len(buf) < int(g.Link.MTU()+EthernetMaximumSize) {
		return 0, errors.New("too short buffer for writing outgoing packets (MTU limited)")
	}

	if pkt = g.Link.Read(); pkt == nil {
		return 0, nil
	} else if pkt.Data().Size()+EthernetMinimumSize > len(buf) {
		return 0, errors.New("outgoing packet exceeds MTU")
	}

	mac := g.Link.LinkAddress()
	n := copy(buf, pkt.EgressRoute.RemoteLinkAddress)
	n += copy(buf[n:], mac)

	binary.BigEndian.PutUint16(buf[n:], uint16(pkt.NetworkProtocolNumber))
	n += 2

	for _, v := range pkt.AsSlices() {
		if n+len(v) > len(buf) {
			return 0, errors.New("bad packet size calculation- exceeds MTU size")
		}

		n += copy(buf[n:], v)
	}

	return n, nil
}

// RecvInboundPacket implements [Stack.RecvInboundPacket].
func (g *GVisorStack) RecvInboundPacket(buf []byte) error {
	hdrLen := EthernetMinimumSize

	if len(buf) < hdrLen {
		return nil
	}

	hdr := buf[0:hdrLen]
	proto := tcpip.NetworkProtocolNumber(binary.BigEndian.Uint16(buf[hdrLen-2 : hdrLen]))
	payload := buf[EthernetMinimumSize:]

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: len(hdr),
		Payload:            buffer.MakeWithData(payload),
	})

	copy(pkt.LinkHeader().Push(len(hdr)), hdr)
	g.Link.InjectInbound(proto, pkt)

	return nil
}

// ListenerTCP4 returns a net.Listener capable of accepting IPv4 TCP
// connections for the argument port on this stack.
func (g *GVisorStack) ListenerTCP4(port uint16) (net.Listener, error) {
	addr, tcpErr := g.Stack.GetMainNICAddress(g.NICID, ipv4.ProtocolNumber)

	if tcpErr != nil {
		return nil, fmt.Errorf("couldn't get NIC IP address: %v", tcpErr)
	}

	fullAddr := tcpip.FullAddress{Addr: addr.Address, Port: port, NIC: g.NICID}
	listener, err := gonet.ListenTCP(g.Stack, fullAddr, ipv4.ProtocolNumber)

	if err != nil {
		return nil, err
	}

	return (net.Listener)(listener), nil
}

// gvisorFullAddr attempts to convert the ip:port to a FullAddress struct.
func gvisorFullAddr(a string) (tcpip.FullAddress, error) {
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
