// VirtIO network driver
//
// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package vnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip/header"

	"github.com/usbarmory/tamago/kvm/virtio"
)

// Device parameters
const (
	DeviceID   = 0x01
	ConfigSize = 12
)

// virtual queue pairs
const (
	// receiveq1
	ReceiveQueue = 0
	// transmitq1
	TransmitQueue = 1
)

// Configuration constants
const (
	STATUS_DOWN = 0
	STATUS_UP   = 1

	SPEED_MIN = 0x00000000
	SPEED_MAX = 0x7fffffff

	DUPLEX_HALF = 0x00
	DUPLEX_FULL = 0x01

	HASH_TYPE_IPV4   = 1 << 0
	HASH_TYPE_TCPV4  = 1 << 1
	HASH_TYPE_UDPV4  = 1 << 2
	HASH_TYPE_IPV6   = 1 << 3
	HASH_TYPE_TCPV6  = 1 << 4
	HASH_TYPE_UDPV6  = 1 << 5
	HASH_TYPE_IP_EX  = 1 << 6
	HASH_TYPE_TCP_EX = 1 << 7
	HASH_TYPE_UDP_EX = 1 << 8
)

// Supported Features
const (
	FeatureChecksum    = (1 << 0)
	FeatureMTU         = (1 << 3)
	FeatureMAC         = (1 << 5)
	FeatureStatus      = (1 << 16)
	FeatureSpeedDuplex = (1 << 63)

	DriverFeatures = FeatureChecksum | FeatureMTU | FeatureMAC | FeatureStatus | FeatureSpeedDuplex
)

// Header flags
const (
	NeedsChecksum = 0
)

// Header represents a VirtIO network device header (virtio_net_hdr)
type Header struct {
	Flags      uint8
	GSOType    uint8
	HdrLen     uint16
	GSOSize    uint16
	CSumStart  uint16
	CSumOffset uint16
	NumBuffers uint16 // not used in legacy drivers
}

const headerLength = 12

// Bytes converts the descriptor structure to byte array format.
func (d *Header) Bytes() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, d)
	return buf.Bytes()
}

// Config represents a VirtIO network device configuration
type Config struct {
	// MAC represents the interface physical address.
	MAC [6]byte
	// Status represents the driver status.
	Status uint16
	// MaxVirtualQueuePairs is the Maximum number of tx/rx queues.
	MaxVirtualQueuePairs uint16
	// MTU represents the Ethernet Maximum Transmission Unit.
	MTU uint16
	// Speed represents the device speed in units of 1Mbps.
	//Speed uint32
	// Duplex represents the communication mode.
	//Duplex uint8
}

// Net represents a VirtIO network device instance.
type Net struct {
	sync.Mutex

	// Controller index
	Index int
	// VirtIO Transport instance
	Transport virtio.VirtIO
	// Interrupt ID
	IRQ int

	// HeaderLength allows to override the VirtIO network device header
	// length as some implementations, such as QEMU, omit certain fields.
	HeaderLength int

	// Maximum Transmission Unit
	MTU uint16

	// receive queue
	rx *virtio.VirtualQueue
	// transmit queue
	tx *virtio.VirtualQueue
}

func (hw *Net) initQueue(index int, flags uint16) (queue *virtio.VirtualQueue) {
	size := hw.Transport.MaxQueueSize(index)
	length := uint16(hw.HeaderLength) + header.EthernetMaximumSize + hw.MTU

	queue = &virtio.VirtualQueue{}
	queue.Init(size, int(length), flags)

	hw.Transport.SetQueueSize(index, size)

	return
}

// Init initializes the VirtIO network device.
func (hw *Net) Init() (err error) {
	hw.Lock()
	defer hw.Unlock()

	if err := hw.Transport.Init(DriverFeatures); err != nil {
		return err
	}

	if id := hw.Transport.DeviceID(); id != DeviceID {
		return fmt.Errorf("incompatible device ID (%x != %x)", id, DeviceID)
	}

	if features := hw.Transport.NegotiatedFeatures(); features&FeatureMTU == FeatureMTU {
		if mtu := hw.Config().MTU; hw.MTU-header.EthernetMaximumSize > mtu {
			return errors.New("incompatible MTU")
		}
	}

	if hw.Transport.QueueReady(ReceiveQueue) || hw.Transport.QueueReady(TransmitQueue) {
		return errors.New("queues unavailable")
	}

	if hw.HeaderLength == 0 {
		hw.HeaderLength = headerLength
	}

	hw.rx = hw.initQueue(ReceiveQueue, virtio.Write)
	hw.tx = hw.initQueue(TransmitQueue, 0)

	return
}

// Config returns the network device configuration.
func (hw *Net) Config() (config Config) {
	if hw.Transport == nil {
		return
	}

	data := hw.Transport.Config(ConfigSize)
	binary.Decode(data, binary.LittleEndian, &config)

	return
}

// Start begins processing of incoming packets.
func (hw *Net) Start() {
	if hw.rx == nil || hw.tx == nil {
		return
	}

	hw.Transport.SetQueue(ReceiveQueue, hw.rx)
	hw.Transport.SetQueue(TransmitQueue, hw.tx)
	hw.Transport.SetReady()

	hw.Transport.QueueNotify(ReceiveQueue)
}

// ReceiveWithHeader receives a single network frame, including the VirtIO
// network device header, excluding the checksum, from the MAC controller ring
// buffer.
func (hw *Net) ReceiveWithHeader(buf []byte) (n int, err error) {
	if n, err = hw.rx.Pop(buf); err != nil {
		return
	}

	if n < hw.HeaderLength {
		return 0, nil
	}

	if hw.rx.NeedsNotify() {
		hw.Transport.QueueNotify(ReceiveQueue)
	}

	return
}

// Receive receives a single network frame, excluding the checksum, from the
// MAC controller ring buffer.
func (hw *Net) Receive(buf []byte) (n int, err error) {
	b := make([]byte, hw.HeaderLength+len(buf))

	if n, err = hw.ReceiveWithHeader(b); n == 0 || err != nil {
		return
	}

	copy(buf, b[hw.HeaderLength:n])
	n -= hw.HeaderLength

	return
}

// Transmit transmits a single network frame, the checksum is appended
// automatically and must not be included.
func (hw *Net) Transmit(buf []byte) (_ error) {
	hdr := make([]byte, hw.HeaderLength)
	buf = append(hdr, buf...)

	hw.tx.Push(buf)

	if hw.rx.NeedsNotify() {
		hw.Transport.QueueNotify(TransmitQueue)
	}

	return
}
