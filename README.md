Bare metal Go TCP/IP connectivity
=================================

This Go package implements TCP/IP connectivity through a generic network
interface to be used with `GOOS=tamago` as supported by the
[TamaGo](https://github.com/usbarmory/tamago) framework for bare metal Go.

The package bridges generic network device and stack interfaces, which on
`GOOS=tamago` can be attached to the Go runtime by setting `net.SocketFunc` to
the interface `Socket` function.

Support for the following pure Go network stacks is provided:

  * [gVisor](https://github.com/usbarmory/go-net/blob/main/gvisor.go) (`go` branch) [tcpip](https://pkg.go.dev/gvisor.dev/gvisor/pkg/tcpip) stack.

The following packages provide compatible network devices:

  * [enet](https://pkg.go.dev/github.com/usbarmory/tamago/soc/nxp/enet): NXP i.MX ENET Ethernet controllers
  * [uefi](https://pkg.go.dev/github.com/usbarmory/go-boot/uefi#SimpleNetwork): UEFI Simple Network
  * [usbnet](https://pkg.go.dev/github.com/usbarmory/go-net/imx-usb): Ethernet over NXP i.MX USB through tamago [nxp/usb](https://pkg.go.dev/github.com/usbarmory/tamago/soc/nxp/usb)
  * [vnet](https://pkg.go.dev/github.com/usbarmory/go-net/virtio): VirtIO network device through tamago [virtio](https://pkg.go.dev/github.com/usbarmory/tamago/kvm/virtio)

Package documentation
=====================

[![Go Reference](https://pkg.go.dev/badge/github.com/usbarmory/go-net.svg)](https://pkg.go.dev/github.com/usbarmory/go-net)

Examples
========

```go
// TamaGo UEFI Simple Network interface
nic, _ := &x64.UEFI.Boot.GetNetwork{}

// gnet interface with gvisor stack
iface := gnet.Interface{
	Stack: NewGVisorStack(1),
}

// initialize IP, MAC, Gateway
_ = iface.Init(nic, "10.0.0.1/24", "", "10.0.0.2")

// Go runtime hook
net.SocketFunc = iface.Stack.Socket
```

UEFI
----

See [go-boot](https://github.com/usbarmory/go-boot/blob/main/cmd/net.go)
for a full integration example with the UEFI Simple Network Protocol.


VirtIO
------

See [tamago-sev-example](https://github.com/usbarmory/tamago-sev-example/blob/main/cmd/net_virtio.go) or
[tamago-example](https://github.com/usbarmory/tamago-example/tree/master/network)
for a full integration example.

NXP i.MX ENET/USB
-----------------

See `mx6ullevk` and `imx8mpevk` (ENET) and `usbarmory` (USB) target support in
[tamago-example](https://github.com/usbarmory/tamago-example/tree/master/network)
for full integration examples.

Authors
=======

Andrea Barisani  
andrea@inversepath.com  

Andrej Rosano  
andrej@inversepath.com  

Patricio Whittingslow  
graded.sp{at}gmail{dot}com

Documentation
=============

The package API documentation can be found on
[pkg.go.dev](https://pkg.go.dev/github.com/usbarmory/go-net).


For more information about TamaGo see its
[repository](https://github.com/usbarmory/tamago) and
[project wiki](https://github.com/usbarmory/tamago/wiki).

License
=======

tamago | https://github.com/usbarmory/go-net  
Copyright (c) The go-net authors. All Rights Reserved.

These source files are distributed under the BSD-style license found in the
[LICENSE](https://github.com/usbarmory/go-net/blob/main/LICENSE) file.
