// Ethernet over i.MX USB driver
//
// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package usbnet

import (
	"errors"
	"net"

	"github.com/usbarmory/tamago/soc/nxp/usb"

	"github.com/usbarmory/go-net"
)

// ECM represents a CDC Ethernet Control Modem (ECM) USB device.
type ECM struct {
	// Host MAC address
	HostMAC net.HardwareAddr

	// Device MAC address
	DeviceMAC net.HardwareAddr

	// Device is the collection of USB device descriptors representing the
	// ECM instance, it is set by [ECM.Init].
	Device *usb.Device

	// Stack represents the associated network stack instance, it must be
	// set before [ECM.Init].
	Stack gnet.Stack

	maxPacketSize int
	out           []byte
	in            []byte
}

// Init initializes a CDC ECM USB device.
func (ecm *ECM) Init() (err error) {
	if ecm.Stack == nil {
		return errors.New("invalid ECM instance")
	}

	// initialize device descriptors
	ecm.Device = &usb.Device{}
	ecm.configureDevice()
	ecm.addControlInterface()
	ecm.addDataInterfaces()

	size := gnet.EthernetMaximumSize + gnet.MTU
	ecm.out = make([]byte, size)
	ecm.in = make([]byte, size)

	return
}

// ECMControl implements the endpoint 2 IN function.
func (ecm *ECM) Control(_ []byte, lastErr error) (in []byte, err error) {
	// ignore for now
	return
}

// ECMRx implements the endpoint 1 OUT function, used to receive Ethernet
// packet from host to device.
func (ecm *ECM) Rx(out []byte, lastErr error) (_ []byte, err error) {
	if len(ecm.out) == 0 && len(out) < 14 {
		return
	}

	ecm.out = append(ecm.out, out...)

	// more data expected or zero length packet
	if len(out) == ecm.maxPacketSize {
		return
	}

	return nil, ecm.Stack.RecvInboundPacket(out)
}

// ECMTx implements the endpoint 1 IN function, used to transmit Ethernet
// packet from device to host.
func (ecm *ECM) Tx(_ []byte, lastErr error) (in []byte, err error) {
	n, err := ecm.Stack.WriteOutboundPacket(ecm.in)

	if n < gnet.EthernetMinimumSize || err != nil {
		return
	}

	return ecm.in[:n], err
}
