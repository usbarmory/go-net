// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package gnet

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/soypat/lneto"
	"github.com/soypat/lneto/x/xnet"
)

// Compile time check to ensure LnetoStack implements Stack interface.
var _ Stack = (*LnetoStack)(nil)

// LnetoConfig provides configuration options to better optimize the LnetoStack.
type LnetoConfig struct {
	// MaxActiveTCPPorts is a heap-memory guardrail to limit number of simultaneous open TCP ports.
	MaxActiveTCPPorts uint16
	// MaxListenerConns limits the amount of open [net.Listener] connections that can be established
	// in simultaneous. Each newly allocated listener conn consumes 2*TCPBufferSize*MaxListenerConns so
	// this can have drastic memory consumption impact.
	MaxListenerConns uint16

	// determine size of each TCP rx/tx ring buffers.
	TCPBufferSize int
	// PollDuration sets the time between protocol checks for completion like DHCP, NTP, DNS etc. via the blocking APIs.
	PollDuration time.Duration
	// TCPQueueSize sets the number of packets that can be sent out and not be acknowledged before halting new packet tx.
	TCPQueueSize int
}

func DefaultLnetoStackConfig() LnetoConfig {
	return LnetoConfig{
		MaxActiveTCPPorts: 1024,
		MaxListenerConns:  32, // Careful with number, large memory impact.
		TCPBufferSize:     2048,
		PollDuration:      time.Millisecond,
	}
}

// NewGVisorStack returns a gvisor stack ready to configure with the given [tcpip.NICID].
func NewLnetoStack(hostname string, cfg LnetoConfig) *LnetoStack {
	if hostname == "" {
		hostname = "gonet-lneto"
	}
	if cfg.PollDuration <= 0 {
		cfg.PollDuration = time.Nanosecond
	}
	return &LnetoStack{
		hostname:         hostname,
		maxTCPPorts:      cfg.MaxActiveTCPPorts,
		maxListenerConns: cfg.MaxListenerConns,
		pollDuration:     cfg.PollDuration,
		tcpBufSize:       cfg.TCPBufferSize,
		tcpQueueSize:     cfg.TCPQueueSize,
	}
}

// LnetoStack implements [Stack] with the [lneto] networking package.
//
// [lneto]: https://github.com/soypat/lneto
type LnetoStack struct {
	hostname         string
	maxTCPPorts      uint16
	maxListenerConns uint16
	pollDuration     time.Duration // Determine poll duration for blocking operations.
	tcpBufSize       int           // determine size of TCP rx/tx ring buffers.
	tcpQueueSize     int
	stack            xnet.StackAsync
	// gostack holds a handle to xnet.StackAsync, it is just a wrapper type.
	gostack     xnet.StackGo
	writenotify func()
}

// Configure sets the NIC ID, MAC address, IP prefix and gateway.
// Gateway may be invalid.
func (ls *LnetoStack) Configure(mac net.HardwareAddr, ip netip.Prefix, gw netip.Addr) error {
	if len(mac) != 6 {
		return errors.New("only MAC address of length 6 supported")
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	stack := &ls.stack
	err := stack.Reset(xnet.StackConfig{
		StaticAddress:     ip.Addr(),
		RandSeed:          int64(binary.LittleEndian.Uint64(rnd[:])),
		MaxActiveTCPPorts: ls.maxTCPPorts,
		MaxActiveUDPPorts: 0, // Unsupported as of yet.
		Hostname:          ls.hostname,
		HardwareAddress:   [6]byte(mac),
		MTU:               uint16(MTU),
		ICMPQueueLimit:    8,
	})
	if err != nil {
		return fmt.Errorf("failed to configure lneto stack: %w", err)
	}
	// Set subnet if set.
	var hostCtl xnet.DHCPResults
	hostCtl.Subnet = ip
	stack.AssimilateDHCPResults(&hostCtl)

	// Prepare socket stack.
	ls.gostack = stack.StackGo(ls.pollDuration, xnet.StackGoConfig{
		ListenerPoolConfig: xnet.TCPPoolConfig{
			PoolSize:           ls.maxTCPPorts,
			QueueSize:          ls.tcpQueueSize,
			TxBufSize:          ls.tcpBufSize,
			RxBufSize:          ls.tcpBufSize,
			EstablishedTimeout: 4 * time.Second,
			ClosingTimeout:     2 * time.Second,
			NanoTime:           nil, // Uses time.Now().UnixNano().
		},
	})
	if gw.IsValid() {
		// We need to discover gateway IP address.
		go func(gw netip.Addr) {
			blocking := stack.StackBlocking(ls.pollDuration)
			hw, err := blocking.DoResolveHardwareAddress6(gw, 5*time.Second)
			if err != nil {
				fmt.Printf("failed to resolve hardware address for %s: %v\n", gw.String(), err)
			} else {
				stack.SetGateway6(hw)
			}
		}(gw)
	}
	return nil
}

// HardwareAddress returns the MAC address of the NIC.
func (ls *LnetoStack) HardwareAddress() (net.HardwareAddr, error) {
	hw := ls.stack.HardwareAddress()
	return hw[:], nil
}

// EnableICMP registers an ICMP handler on the stack.
func (ls *LnetoStack) EnableICMP() error {
	return ls.stack.EnableICMP(true)
}

// Socket creates a network socket bound to laddr and connected to raddr.
func (ls *LnetoStack) Socket(ctx context.Context, network string, family, sotype int, laddr, raddr net.Addr) (c interface{}, err error) {
	if ls.writenotify != nil {
		defer ls.writenotify()
	}
	return ls.gostack.Socket(ctx, network, family, sotype, laddr, raddr)
}

// SetWriteNotify registers a callback invoked when outbound data is ready.
func (ls *LnetoStack) SetWriteNotify(cb func()) {
	ls.writenotify = cb
}

// WriteOutboundPacket dequeues one outbound packet into buf, returning bytes written.
func (ls *LnetoStack) WriteOutboundPacket(buf []byte) (int, error) {
	return ls.stack.EgressEthernet(buf)
}

// RecvInboundPacket delivers an inbound packet to the stack.
func (ls *LnetoStack) RecvInboundPacket(buf []byte) error {
	err := ls.stack.IngressEthernet(buf)
	if err != lneto.ErrPacketDrop && ls.writenotify != nil {
		ls.writenotify()
	}
	return err
}
