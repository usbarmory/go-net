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
func (stack *GVisorStack) HardwareAddress() (net.HardwareAddr, error) {
	addr := stack.Link.LinkAddress()
	return net.HardwareAddr(addr), nil
}

// Configure implements [Stack.Configure].
func (iface *GVisorStack) Configure(mac net.HardwareAddr, ip netip.Prefix, gw netip.Addr) (err error) {
	linkAddr := tcpip.LinkAddress(mac)
	if iface.NICID == 0 {
		iface.NICID = tcpip.NICID(NICID)
	}

	if iface.Stack == nil {
		iface.Stack = stack.New(DefaultStackOptions)
	}

	iface.Link = channel.New(256, MTU, linkAddr)
	iface.Link.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	linkEP := stack.LinkEndpoint(iface.Link)

	if err := iface.Stack.CreateNIC(iface.NICID, linkEP); err != nil {
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

	if err := iface.Stack.AddProtocolAddress(iface.NICID, protocolAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("%v", err)
	}

	rt := iface.Stack.GetRouteTable()
	rt = append(rt, tcpip.Route{
		Destination: protocolAddr.AddressWithPrefix.Subnet(),
		NIC:         iface.NICID,
	})

	var gwaddr tcpip.Address

	if gw.IsValid() {
		gwaddr = tcpip.AddrFromSlice(net.ParseIP(gw.String())).To4()
	}

	rt = append(rt, tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		Gateway:     gwaddr,
		NIC:         iface.NICID,
	})

	iface.Stack.SetRouteTable(rt)

	return nil
}

// EnableICMP implements [Stack].
func (stack *GVisorStack) EnableICMP() error {
	var wq waiter.Queue

	ep, err := stack.Stack.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)

	if err != nil {
		return fmt.Errorf("endpoint error (icmp): %v", err)
	}

	addr, tcpErr := stack.Stack.GetMainNICAddress(stack.NICID, ipv4.ProtocolNumber)

	if tcpErr != nil {
		return fmt.Errorf("couldn't get NIC IP address: %v", tcpErr)
	}

	fullAddr := tcpip.FullAddress{Addr: addr.Address, Port: 0, NIC: stack.NICID}

	if err := ep.Bind(fullAddr); err != nil {
		return fmt.Errorf("bind error (icmp endpoint): %s", err)
	}

	return nil
}

// Socket implements [Stack.Socket].
func (stack *GVisorStack) Socket(ctx context.Context, network string, family, sotype int, laddr, raddr net.Addr) (c interface{}, err error) {
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
		c, err = gonet.DialUDP(stack.Stack, &lFullAddr, &rFullAddr, proto)
	case "tcp", "tcp4":
		if sotype != syscall.SOCK_STREAM {
			return nil, errors.New("unsupported socket type")
		}

		if raddr != nil {
			c, err = gonet.DialContextTCP(ctx, stack.Stack, rFullAddr, proto)
		} else {
			c, err = gonet.ListenTCP(stack.Stack, lFullAddr, proto)
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
func (stack *GVisorStack) SetWriteNotify(notifier func()) {
	if stack.prevNotify != nil {
		stack.Link.RemoveNotify(stack.prevNotify)
	}
	stack.prevNotify = stack.Link.AddNotify(writeNotifierFunc(notifier))
}

// WriteOutboundPacket implements [Stack.WriteOutboundPacket].
func (iface *GVisorStack) WriteOutboundPacket(buf []byte) (int, error) {
	var pkt *stack.PacketBuffer

	if len(buf) < int(iface.Link.MTU()+EthernetMaximumSize) {
		return 0, errors.New("too short buffer for writing outgoing packets (MTU limited)")
	}

	if pkt = iface.Link.Read(); pkt == nil {
		return 0, nil
	} else if pkt.Data().Size()+EthernetMinimumSize > len(buf) {
		return 0, errors.New("outgoing packet exceeds MTU")
	}

	mac := iface.Link.LinkAddress()
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
func (iface *GVisorStack) RecvInboundPacket(buf []byte) error {
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
	iface.Link.InjectInbound(proto, pkt)

	return nil
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
