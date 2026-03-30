package gnet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type netstack struct {
	Stack *stack.Stack
	Link  *channel.Endpoint
	NIDID tcpip.NICID
}

func (iface *netstack) NICID() uint32 {
	return uint32(iface.NIDID)
}

func (iface *netstack) configure(nicid uint32, mac string, ip netip.Prefix, gw netip.Addr) (err error) {
	linkAddr, err := tcpip.ParseMACAddress(mac)
	if err != nil {
		return
	}
	iface.NIDID = tcpip.NICID(nicid)
	if iface.Stack == nil {
		iface.Stack = stack.New(DefaultStackOptions)
	}

	iface.Link = channel.New(256, MTU, linkAddr)
	iface.Link.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	linkEP := stack.LinkEndpoint(iface.Link)

	if err := iface.Stack.CreateNIC(iface.NIDID, linkEP); err != nil {
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

	if err := iface.Stack.AddProtocolAddress(iface.NIDID, protocolAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("%v", err)
	}

	rt := iface.Stack.GetRouteTable()

	rt = append(rt, tcpip.Route{
		Destination: protocolAddr.AddressWithPrefix.Subnet(),
		NIC:         iface.NIDID,
	})
	var gwaddr tcpip.Address
	if gw.IsValid() {
		gwaddr = tcpip.AddrFromSlice(net.ParseIP(gw.String())).To4()
	}

	rt = append(rt, tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		Gateway:     gwaddr,
		NIC:         iface.NIDID,
	})

	iface.Stack.SetRouteTable(rt)
	return nil
}

// EnableICMP adds an ICMP endpoint to the interface, it is useful to enable
// ping requests.
func (iface *netstack) EnableICMP() error {
	var wq waiter.Queue

	ep, err := iface.Stack.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)

	if err != nil {
		return fmt.Errorf("endpoint error (icmp): %v", err)
	}

	addr, tcpErr := iface.Stack.GetMainNICAddress(iface.NIDID, ipv4.ProtocolNumber)

	if tcpErr != nil {
		return fmt.Errorf("couldn't get NIC IP address: %v", tcpErr)
	}

	fullAddr := tcpip.FullAddress{Addr: addr.Address, Port: 0, NIC: iface.NIDID}

	if err := ep.Bind(fullAddr); err != nil {
		return fmt.Errorf("bind error (icmp endpoint): ", err)
	}

	return nil
}

// Socket can be used as net.SocketFunc under GOOS=tamago to allow its use
// internal use within the Go runtime.
func (iface *netstack) Socket(ctx context.Context, network string, family, sotype int, laddr, raddr net.Addr) (c interface{}, err error) {
	var proto tcpip.NetworkProtocolNumber
	var lFullAddr tcpip.FullAddress
	var rFullAddr tcpip.FullAddress

	if laddr != nil {
		if lFullAddr, err = fullAddr(laddr.String()); err != nil {
			return
		}
	}

	if raddr != nil {
		if rFullAddr, err = fullAddr(raddr.String()); err != nil {
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

		if c, err = gonet.DialUDP(iface.Stack, &lFullAddr, &rFullAddr, proto); c != nil {
			return
		}
	case "tcp", "tcp4":
		if sotype != syscall.SOCK_STREAM {
			return nil, errors.New("unsupported socket type")
		}

		if raddr != nil {
			if c, err = gonet.DialContextTCP(ctx, iface.Stack, rFullAddr, proto); err != nil {
				return
			}
		} else {
			if c, err = gonet.ListenTCP(iface.Stack, lFullAddr, proto); err != nil {
				return
			}
		}
	default:
		return nil, errors.New("unsupported network")
	}

	return
}

func (iface *netstack) AddNotify(notifier writeNotifier) {
	iface.Link.AddNotify(notifier)
}

func (iface *netstack) WriteOutboundPacket(buf []byte) (int, error) {
	if len(buf) < int(iface.Link.MTU()+18) {
		return 0, errors.New("too short buffer for writing outgoing packets (MTU limited)")
	}
	var pkt *stack.PacketBuffer
	if pkt = iface.Link.Read(); pkt == nil {
		return 0, nil
	} else if pkt.Data().Size()+14 > len(buf) {
		return 0, errors.New("outgoing packet exceeds MTU")
	}

	proto := make([]byte, 2)
	binary.BigEndian.PutUint16(proto, uint16(pkt.NetworkProtocolNumber))

	// Ethernet frame header
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

func (iface *netstack) ReadInboundPacket(buf []byte) error {
	if len(buf) < 14 {
		return nil
	}
	hdr := buf[0:14]
	proto := tcpip.NetworkProtocolNumber(binary.BigEndian.Uint16(buf[12:14]))
	payload := buf[14:]
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: len(hdr),
		Payload:            buffer.MakeWithData(payload),
	})
	copy(pkt.LinkHeader().Push(len(hdr)), hdr)
	iface.Link.InjectInbound(proto, pkt)
	return nil
}
