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
	"runtime"
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

	// MaxActiveTCPPorts is a heap-memory guardrail to limit number of
	// simultaneous open TCP ports.
	MaxActiveTCPPorts uint16

	// MaxListenerConns limits the amount of open [net.Listener]
	// connections that can be established in simultaneous. Each newly
	// allocated listener conn consumes 2*TCPBufferSize*MaxListenerConns so
	// this can have drastic memory consumption impact.
	MaxListenerConns uint16

	// TCPBufferSize determines the size of each TCP rx/tx ring buffers.
	TCPBufferSize int

	// BackoffStack sets the time between protocol checks for completion
	// like DHCP, NTP, DNS etc. via the blocking APIs.  BackoffStack can
	// use a channel driven approach behind the scenes and return
	// [lneto.BackoffFlagNop] to signal backoff yield is implemented by the
	// callback and that no sleep should be performed.
	BackoffStack lneto.BackoffStrategy

	// NewBackoffTCP returns a per-connection backoff strategy for TCP
	// read/write retries.  If nil, a short-exponential default is used (or
	// [lneto.BackoffFlagGosched] on GOMAXPROCS=1).
	NewBackoffTCP func() lneto.BackoffStrategy

	// TCPQueueSize sets the number of packets that can be sent out and not
	// be acknowledged before halting new packet tx.
	TCPQueueSize int

	// ManualPoll disables the goroutine draining outbound packets through
	// the write notification callback, the stack is driven by its user
	// through the non-blocking [LnetoStack.Async], with outbound packets
	// transmitted by [Interface.Flush] (or dequeued by
	// [LnetoStack.WriteOutboundPacket]). Blocking operations, such as
	// [LnetoStack.Socket], need those packets to be drained.
	ManualPoll bool
}

// DefaultLnetoStackConfig returns an [LnetoConfig] ready for use with
// [NewLnetoStack] with sane configuration parameters.
func DefaultLnetoStackConfig() *LnetoConfig {
	// do not consider IP+TCP headers
	const tcpMaxSize = MTU - 20 - 20

	return &LnetoConfig{
		MaxActiveTCPPorts: 16,
		MaxListenerConns:  32,             // Careful with number, large memory impact.
		TCPBufferSize:     3 * tcpMaxSize, // 3× seems to work good on cyw43439.
		TCPQueueSize:      8,
		// Backoffs are nil here, they are automatically set in NewLnetoStack.
		BackoffStack:  nil,
		NewBackoffTCP: nil,
	}
}

// NewLnetoStack returns a stack using the [Lneto] userspace networking
// library.
//
// [Lneto]: https://github.com/soypat/lneto
func NewLnetoStack(cfg *LnetoConfig) *LnetoStack {
	if cfg == nil {
		cfg = DefaultLnetoStackConfig()
	}

	if cfg.BackoffStack == nil {
		if runtime.GOMAXPROCS(0) == 1 {

			// to throttle a busy loop; sleeping starves the NIC
			// poll → frame loss. Gosched keeps the poll continuous
			// while still yielding cooperatively.
			cfg.BackoffStack = backoffYield
		} else {
			cfg.BackoffStack = defaultStackBackoff
		}
	}

	if cfg.NewBackoffTCP == nil {
		if runtime.GOMAXPROCS(0) == 1 {
			cfg.NewBackoffTCP = func() lneto.BackoffStrategy {
				return backoffYield
			}
		} else {
			cfg.NewBackoffTCP = defaultNewTCPBackoff
		}
	}

	if cfg.Hostname == "" {
		cfg.Hostname = "gonet-lneto"
	}

	irq, backoff := interruptBackoff(cfg.BackoffStack)
	ls := &LnetoStack{
		manualPoll:       cfg.ManualPoll,
		hostname:         cfg.Hostname,
		maxTCPPorts:      cfg.MaxActiveTCPPorts,
		maxListenerConns: cfg.MaxListenerConns,
		tcpBufSize:       cfg.TCPBufferSize,
		tcpQueueSize:     cfg.TCPQueueSize,
		backoff:          backoff,
		backoffirq:       irq,
		tcpbackoff:       cfg.NewBackoffTCP,
	}
	return ls
}

// LnetoStack implements [Stack] with the [lneto] networking package.
//
// [lneto]: https://github.com/soypat/lneto
type LnetoStack struct {
	manualPoll       bool
	hostname         string
	maxTCPPorts      uint16
	maxListenerConns uint16
	backoff          lneto.BackoffStrategy
	backoffirq       chan<- event
	tcpbackoff       func() lneto.BackoffStrategy
	tcpBufSize       int
	tcpQueueSize     int
	stack            xnet.StackAsync
	gostack          xnet.StackGo
	_writenotify     func(buf []byte)
	goroutineID      atomic.Uint32
}

// Configure sets the MAC address, IP prefix and gateway.
// Gateway may be invalid.
func (ls *LnetoStack) Configure(mac net.HardwareAddr, ip netip.Prefix, gw netip.Addr) (err error) {
	if len(mac) != 6 {
		return errors.New("only MAC address of length 6 supported")
	}

	rnd := make([]byte, 8)
	rand.Read(rnd)

	cfg := xnet.StackConfig{
		RandSeed:          int64(binary.LittleEndian.Uint64(rnd[:])),
		MaxActiveTCPPorts: ls.maxTCPPorts,
		MaxActiveUDPPorts: 0, // Unsupported as of yet.
		Hostname:          ls.hostname,
		HardwareAddress:   [6]byte(mac),
		MTU:               uint16(MTU),
		ICMPQueueLimit:    32,
	}

	if ip.Addr().Is4() {
		cfg.StaticAddress4 = ip.Addr().As4()
	} else if ip.Addr().Is6() {
		cfg.StaticAddress6 = ip.Addr().As16()
		cfg.IPv6Stack = xnet.DefaultStack6()
	}

	ls.goroutineID.Add(1)
	stack := &ls.stack

	if err = stack.Reset(cfg); err != nil {
		return fmt.Errorf("failed to configure lneto stack: %w", err)
	}

	var hostCtl xnet.DHCPResults
	hostCtl.Subnet = ip
	stack.AssimilateDHCPResults(&hostCtl)

	ls.gostack = stack.StackGo(ls.backoff, xnet.StackGoConfig{
		ListenerPoolConfig: xnet.TCPPoolConfig{
			PoolSize:           ls.maxTCPPorts,
			QueueSize:          ls.tcpQueueSize,
			TxBufSize:          ls.tcpBufSize,
			RxBufSize:          ls.tcpBufSize,
			EstablishedTimeout: 4 * time.Second,
			ClosingTimeout:     2 * time.Second,
			NanoTime:           nil,
			NewBackoff:         ls.tcpbackoff,
		},
	})

	if !ls.manualPoll {
		go ls.lifetimeGoroutine(ls.goroutineID.Add(1))
	}

	if gw.IsValid() {
		// We need to discover gateway IP address.
		go ls.resolveSetGateway(gw)
	}

	return nil
}

// Async returns the underlying non-blocking lneto stack, valid after
// [LnetoStack.Configure], for use without [LnetoStack.Socket] (see
// [LnetoConfig.ManualPoll]).
func (ls *LnetoStack) Async() *xnet.StackAsync {
	return &ls.stack
}

// HardwareAddress returns the MAC address of the NIC.
func (ls *LnetoStack) HardwareAddress() (net.HardwareAddr, error) {
	hw := ls.stack.HardwareAddr()
	return hw[:], nil
}

// EnableICMP registers an ICMP handler on the stack.
func (ls *LnetoStack) EnableICMP() error {
	return ls.stack.EnableICMP(true)
}

// Socket creates a network socket bound to laddr and connected to raddr.
func (ls *LnetoStack) Socket(ctx context.Context, network string, family, sotype int, laddr, raddr net.Addr) (c interface{}, err error) {
	ls.interruptBackoff()       // wake lifetimeGoroutine: packets will be queued immediately
	defer ls.interruptBackoff() // wake again on return so sleepers react to completion
	return ls.gostack.Socket(ctx, network, family, sotype, laddr, raddr)
}

// SetWriteNotify registers a callback invoked when outbound data is ready.
func (ls *LnetoStack) SetWriteNotify(cb func(buf []byte)) {
	ls._writenotify = cb
}

func (ls *LnetoStack) tryWriteNotify(gid uint32, buf []byte) {
	wn := ls._writenotify
	if wn != nil && ls.goroutineID.Load() == gid {
		wn(buf)
	}
}

// WriteOutboundPacket dequeues one outbound packet into buf, returning bytes written.
func (ls *LnetoStack) WriteOutboundPacket(buf []byte) (int, error) {
	n, err := ls.stack.EgressEthernet(buf)
	if n > 0 {
		// Only interrupt when a packet was actually dequeued. An
		// unconditional interrupt here would re-fill irq every idle
		// poll, making backoff.Do a no-op and creating a busy-loop
		// that starves other goroutines under GOMAXPROCS=1 (bare-metal
		// cooperative scheduling).
		ls.interruptBackoff()
	}
	return n, err
}

// RecvInboundPacket delivers an inbound packet to the stack.
func (ls *LnetoStack) RecvInboundPacket(buf []byte) error {
	defer ls.interruptBackoff()
	return ls.stack.IngressEthernet(buf)
}

func (ls *LnetoStack) resolveSetGateway(gw netip.Addr) (err error) {
	blocking := ls.stack.StackRetrying(ls.backoff)
	hw, err := blocking.DoResolveHardwareAddress6(gw, 4*time.Second, 3)

	if err != nil {
		fmt.Printf("failed to resolve gateway hardware address for %s: %v\n", gw.String(), err)
		return err
	}

	ls.stack.SetGatewayHardwareAddr(hw)
	return nil
}

// lifetimeGoroutine drives the write-notify callback on a fixed cadence. The
// lneto stack queues outbound frames internally during blocking operations
// (ARP resolution, TCP handshake, retransmits) but, unlike gVisor's
// channel.Endpoint, has no built-in hook to notify the host of a new egress
// packet. Without this ticker the [Interface] would never drain the queue
// while a Socket call is blocking. Each Configure call bumps goroutineID and
// starts a fresh ticker; any previously-running ticker observes the mismatch
// on its next iteration and exits, so at most one ticker is live per stack.
func (ls *LnetoStack) lifetimeGoroutine(id uint32) {
	var stats xnet.Statistics
	backoff := ls.backoff
	if backoff == nil {
		backoff = defaultStackBackoff
	}
	writeBuf := make([]byte, MTU+EthernetMaximumSize)
	var backoffs uint
	for id == ls.goroutineID.Load() {
		ls.stack.ReadStatistics(&stats)
		sent := stats.TotalSent
		ls.tryWriteNotify(id, writeBuf)
		if id != ls.goroutineID.Load() {
			break
		}
		ls.stack.ReadStatistics(&stats)
		if sent != stats.TotalSent {
			backoffs = 0 // packet sent — stay eager
			runtime.Gosched()
		} else {
			backoff.Do(backoffs) // idle — back off
			backoffs++
		}
	}
}

func (ls *LnetoStack) interruptBackoff() {
	select {
	case ls.backoffirq <- event{}:
	default:
	}
}

type event struct{}

// interruptBackoff takes an existing backoff strategy and makes it
// interruptible via write to channel.  The returned strategy is safe to call
// from multiple goroutines concurrently: each call creates its own timer, so
// goroutines do not race on a shared ticker. The irq channel (capacity 1) is
// shared; one interrupt wakes exactly one sleeping caller, which is the
// intended best-effort behavior.
func interruptBackoff(backoff lneto.BackoffStrategy) (interrupt chan<- event, _ lneto.BackoffStrategy) {
	irq := make(chan event, 1)
	interruptibleBackoff := func(consecutiveBackoffs uint) (sleepOrFlag time.Duration) {
		sleepOrFlag = backoff(consecutiveBackoffs)
		switch sleepOrFlag {
		case lneto.BackoffFlagGosched:
			runtime.Gosched()
		case lneto.BackoffFlagNop:
			// do nothing
		default:
			timer := time.NewTimer(sleepOrFlag) // per-call: goroutines must not share a timer
			select {
			case <-irq:
				if !timer.Stop() && len(timer.C) > 0 {
					<-timer.C
				}
			case <-timer.C:
			}
		}
		return lneto.BackoffFlagNop // yield handled entirely by this callback, signal caller to do nothing
	}
	return irq, interruptibleBackoff
}

// defaultStackBackoff returns a backoff duration for stack protocol retry
// loops.  This strategy is meant for DHCP,NTP- not for stream protocols like
// TCP.
func defaultStackBackoff(consecutiveBackoffs uint) time.Duration {
	const (
		// stay in 32bit space for faster operations
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

func defaultNewTCPBackoff() lneto.BackoffStrategy {
	return defaultTCPBackoff
}

// defaultTCPBackoff returns a per-connection read/write retry backoff for TCP
// stream protocols.  Shorter range than defaultStackBackoff to keep
// interactive sessions (e.g. SSH) responsive.
func defaultTCPBackoff(consecutiveBackoffs uint) time.Duration {
	const (
		minWait                   = 10 * time.Microsecond
		maxWait                   = 1 * time.Millisecond
		maxShift                  = 10
		_compileTimeOverflowCheck = minWait << maxShift
	)

	sleep := minWait << min(consecutiveBackoffs, maxShift)

	if sleep > maxWait {
		sleep = maxWait
	}

	return time.Duration(sleep)
}

func backoffYield(consecutiveBackoffs uint) time.Duration {
	return lneto.BackoffFlagGosched
}
