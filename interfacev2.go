package gnet

import (
	"crypto/rand"
	"net"
	"net/netip"
	"runtime"
)

type InterfaceV2 struct {
	lli            netstack
	nic            NetworkDevice
	HandleStackErr func(error)
}

func (iface *InterfaceV2) Init(nic NetworkDevice, addr string, mac string, gateway string) (err error) {
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

	if err = iface.lli.configure(uint32(NICID), laddr.String(), pfx, gwaddr); err != nil {
		return err
	}
	iface.nic = nic
	iface.lli.AddNotify(iface)
	return nil
}

type writeNotifier interface {
	WriteNotify()
}

func (iface *InterfaceV2) EnableICMP() error {
	return iface.lli.EnableICMP()
}

func (iface *InterfaceV2) WriteNotify() {
	buf := make([]byte, MTU+18)
	n, err := iface.lli.WriteOutboundPacket(buf)
	if err != nil {
		iface.HandleStackErr(err)
		return
	} else if n < 14 {
		return
	}
	iface.nic.Transmit(buf[:n])
}

// StartRx begins processing of incoming packets, the function receives packets
// through [NetworkDevice.Receive] and handles them through [NIC.Rx], it should
// never return.
func (iface *InterfaceV2) StartRx() {
	buf := make([]byte, MTU)
	for {
		if n, err := iface.nic.Receive(buf); err == nil && n > 0 {
			err = iface.lli.ReadInboundPacket(buf[:n])
			if err != nil {
				iface.HandleStackErr(err)
			}
		} else {
			runtime.Gosched()
		}
	}
}
