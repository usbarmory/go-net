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
	"sync/atomic"
	"time"

	"github.com/soypat/lneto"
	"github.com/soypat/lneto/x/xnet"
)

// Compile time check to ensure LnetoStack implements Stack interface.
var _ Stack = (*LnetoStack)(nil)

// LnetoConfig provides configuration options to better optimize the LnetoStack.
type LnetoConfig struct {
	// Hostname specifies the hostname to use for DHCP. Optional.
	Hostname string
	// MaxActiveTCPPorts is a heap-memory guardrail to limit number of simultaneous open TCP ports.
	MaxActiveTCPPorts uint16
	// MaxListenerConns limits the amount of open [net.Listener] connections that can be established
	// in simultaneous. Each newly allocated listener conn consumes 2*TCPBufferSize*MaxListenerConns so
	// this can have drastic memory consumption impact.
	MaxListenerConns uint16

	// determine size of each TCP rx/tx ring buffers.
	TCPBufferSize int
	// BackoffStack sets the time between protocol checks for completion like DHCP, NTP, DNS etc. via the blocking APIs.
	// BackoffStack can use a channel driven approach behind the scenes
	// and return [lneto.BackoffFlagNop] to signal backoff yield is
	// implemented by the callback and that no sleep should be performed.
	BackoffStack lneto.BackoffStrategy
	// TCPQueueSize sets the number of packets that can be sent out and not be acknowledged before halting new packet tx.
	TCPQueueSize int
}

// DefaultLnetoStackConfig returns an [LnetoConfig] ready for use with [NewLnetoStack]
// with sane configuration parameters.
func DefaultLnetoStackConfig() *LnetoConfig {
	const tcpMaxSize = MTU - 20 - 20 // Do not consider IP+TCP headers.
	return &LnetoConfig{
		MaxActiveTCPPorts: 16,
		MaxListenerConns:  32,             // Careful with number, large memory impact.
		TCPBufferSize:     3 * tcpMaxSize, // 3× seems to work good on cyw43439.
		TCPQueueSize:      8,
		BackoffStack:      defaultStackBackoff,
	}
}

// NewLnetoStack returns a stack using the [Lneto] userspace networking library.
//
// [Lneto]: https://github.com/soypat/lneto
func NewLnetoStack(cfg *LnetoConfig) *LnetoStack {
	if cfg.Hostname == "" {
		cfg.Hostname = "gonet-lneto"
	}
	if cfg == nil {
		cfg = DefaultLnetoStackConfig()
	}
	return &LnetoStack{
		hostname:         cfg.Hostname,
		maxTCPPorts:      cfg.MaxActiveTCPPorts,
		maxListenerConns: cfg.MaxListenerConns,
		backoff:          cfg.BackoffStack,
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
	backoff          lneto.BackoffStrategy // Determine poll duration for blocking operations.
	tcpBufSize       int                   // determine size of TCP rx/tx ring buffers.
	tcpQueueSize     int
	stack            xnet.StackAsync
	// gostack holds a handle to xnet.StackAsync, it is just a wrapper type.
	gostack      xnet.StackGo
	_writenotify func()
	goroutineID  atomic.Uint32
}

// Configure sets the MAC address, IP prefix and gateway.
// Gateway may be invalid.
func (ls *LnetoStack) Configure(mac net.HardwareAddr, ip netip.Prefix, gw netip.Addr) error {
	if len(mac) != 6 {
		return errors.New("only MAC address of length 6 supported")
	}
	// Invalidate existing goroutine before resetting stack.
	ls.goroutineID.Add(1)
	rnd := make([]byte, 8)
	rand.Read(rnd)
	stack := &ls.stack
	err := stack.Reset(xnet.StackConfig{
		StaticAddress:     ip.Addr(),
		RandSeed:          int64(binary.LittleEndian.Uint64(rnd)),
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
	ls.gostack = stack.StackGo(ls.backoff, xnet.StackGoConfig{
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

	go ls.lifetimeGoroutine(ls.goroutineID.Add(1))
	if gw.IsValid() {
		// We need to discover gateway IP address.
		go ls.resolveSetGateway(gw)
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
	defer ls.tryWriteNotify(ls.goroutineID.Load())
	return ls.gostack.Socket(ctx, network, family, sotype, laddr, raddr)
}

// SetWriteNotify registers a callback invoked when outbound data is ready.
func (ls *LnetoStack) SetWriteNotify(cb func()) {
	ls._writenotify = cb
}

func (ls *LnetoStack) tryWriteNotify(gid uint32) {
	wn := ls._writenotify
	if wn != nil && ls.goroutineID.Load() == gid {
		wn()
	}
}

// WriteOutboundPacket dequeues one outbound packet into buf, returning bytes written.
func (ls *LnetoStack) WriteOutboundPacket(buf []byte) (int, error) {
	return ls.stack.EgressEthernet(buf)
}

// RecvInboundPacket delivers an inbound packet to the stack.
func (ls *LnetoStack) RecvInboundPacket(buf []byte) error {
	err := ls.stack.IngressEthernet(buf)
	if err != lneto.ErrPacketDrop {
		ls.tryWriteNotify(ls.goroutineID.Load())
	}
	return err
}

func (ls *LnetoStack) resolveSetGateway(gw netip.Addr) (err error) {
	blocking := ls.stack.StackBlocking(ls.backoff)
	hw, err := blocking.DoResolveHardwareAddress6(gw, 5*time.Second)
	if err != nil {
		fmt.Printf("failed to resolve hardware address for %s: %v\n", gw.String(), err)
		return err
	}
	ls.stack.SetGateway6(hw)
	return nil
}

// lifetimeGoroutine drives the write-notify callback on a fixed cadence. The lneto stack
// queues outbound frames internally during blocking operations (ARP
// resolution, TCP handshake, retransmits) but, unlike gVisor's
// channel.Endpoint, has no built-in hook to notify the host of a new
// egress packet. Without this ticker the [Interface] would never drain
// the queue while a Socket call is blocking. Each Configure call bumps
// goroutineID and starts a fresh ticker; any previously-running ticker
// observes the mismatch on its next iteration and exits, so at most one
// ticker is live per stack.
func (ls *LnetoStack) lifetimeGoroutine(id uint32) {
	var stats xnet.Statistics
	backoff := ls.backoff
	if backoff == nil {
		backoff = defaultStackBackoff
	}
	var backoffs uint
	for id == ls.goroutineID.Load() {
		ls.stack.ReadStatistics(&stats)
		sent := stats.TotalSent
		ls.tryWriteNotify(id)
		if id != ls.goroutineID.Load() {
			break
		}
		ls.stack.ReadStatistics(&stats)
		if sent != stats.TotalSent {
			backoff.Do(backoffs)
			backoffs++
		} else {
			backoffs = 0
		}
	}
}

// defaultStackBackoff returns a backoff duration for stack protocol retry loops.
// This strategy is meant for DHCP,NTP- not for stream protocols like TCP.
func defaultStackBackoff(consecutiveBackoffs uint) time.Duration {
	const (
		// Stay in 32bit space for faster operations.
		minWait = 100 * time.Microsecond
		maxWait = 20 * time.Millisecond

		maxShift                  = 15
		_compileTimeOverflowCheck = minWait << maxShift
	)
	sleep := minWait << min(consecutiveBackoffs, maxShift)
	if sleep > maxWait {
		sleep = maxWait
	}
	return time.Duration(sleep)
}
