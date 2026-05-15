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
	if cfg == nil {
		cfg = DefaultLnetoStackConfig()
	}
	if cfg.BackoffStack == nil {
		if runtime.GOMAXPROCS(0) == 1 {
			// On a single-threaded target there is no OS scheduler to throttle
			// a busy loop; sleeping starves the NIC poll → frame loss. Gosched
			// keeps the poll continuous while still yielding cooperatively.
			cfg.BackoffStack = func(_ uint) time.Duration {
				return lneto.BackoffFlagGosched
			}
		} else {
			cfg.BackoffStack = defaultStackBackoff
		}
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "gonet-lneto"
	}
	irq, backoff := interruptBackoff(cfg.BackoffStack)
	return &LnetoStack{
		hostname:         cfg.Hostname,
		maxTCPPorts:      cfg.MaxActiveTCPPorts,
		maxListenerConns: cfg.MaxListenerConns,
		tcpBufSize:       cfg.TCPBufferSize,
		tcpQueueSize:     cfg.TCPQueueSize,
		backoff:          backoff,
		backoffirq:       irq,
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
	backoffirq       chan<- event
	tcpBufSize       int // determine size of TCP rx/tx ring buffers.
	tcpQueueSize     int
	stack            xnet.StackAsync
	// gostack holds a handle to xnet.StackAsync, it is just a wrapper type.
	gostack      xnet.StackGo
	_writenotify func(buf []byte)
	goroutineID  atomic.Uint32
}

// Configure sets the MAC address, IP prefix and gateway.
// Gateway may be invalid.
func (ls *LnetoStack) Configure(mac net.HardwareAddr, ip netip.Prefix, gw netip.Addr) error {
	if len(mac) != 6 {
		return errors.New("only MAC address of length 6 supported")
	}
	// Invalidate existing goroutine before resetting stack.\
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
	err := stack.Reset(cfg)
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
		// Only interrupt when a packet was actually dequeued. An unconditional
		// interrupt here would re-fill irq every idle poll, making backoff.Do
		// a no-op and creating a busy-loop that starves other goroutines under
		// GOMAXPROCS=1 (bare-metal cooperative scheduling).
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
	blocking := ls.stack.StackBlocking(ls.backoff)
	hw, err := blocking.DoResolveHardwareAddress6(gw, 5*time.Second)
	if err != nil {
		fmt.Printf("failed to resolve hardware address for %s: %v\n", gw.String(), err)
		return err
	}
	ls.stack.SetGatewayHardwareAddr(hw)
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

// interruptBackoff takes an existing backoff strategy and makes it interruptible via write to channel.
// The returned strategy is safe to call from multiple goroutines concurrently: each call creates its
// own timer, so goroutines do not race on a shared ticker. The irq channel (capacity 1) is shared;
// one interrupt wakes exactly one sleeping caller, which is the intended best-effort behavior.
func interruptBackoff(backoff lneto.BackoffStrategy) (interrupt chan<- event, _ lneto.BackoffStrategy) {
	irq := make(chan event, 1)
	interruptibleBackoff := func(consecutiveBackoffs uint) (sleepOrFlag time.Duration) {
		sleepOrFlag = backoff(consecutiveBackoffs)
		switch sleepOrFlag {
		case lneto.BackoffFlagGosched:
			runtime.Gosched()
		case lneto.BackoffFlagNop:
			// Do nothing.
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
		return lneto.BackoffFlagNop // Yield handled entirely by this callback; signal caller to do nothing.
	}
	return irq, interruptibleBackoff
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
