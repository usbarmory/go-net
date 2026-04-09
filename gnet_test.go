// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package gnet

import (
	"context"
	"net"
)

func ExampleInterface() {
	const (
		addr    = "10.0.0.1/24"
		gateway = "10.0.0.2"
		mac     = ""

		remoteAddr = "10.0.0.3"
		remotePort = 80

		sockStream = 0x1
		sockDgram  = 0x2
		familyInet = 0x1
	)
	var nic NetworkDevice = nil // Need a device implementation.
	netstack := NewDefaultStack()

	var iface Interface
	err := iface.Init(nic, netstack, addr, mac, gateway)
	if err != nil {
		panic(err)
	}
	go iface.StartRx()

	raddr := &net.TCPAddr{
		IP:   net.ParseIP(remoteAddr),
		Port: remotePort,
	}
	c, err := netstack.Socket(context.Background(), "tcp", familyInet, sockStream, nil, raddr)
	if err != nil {
		panic(err)
	}
	conn := c.(net.Conn)
	// Use conn...
	conn.Close()
}
