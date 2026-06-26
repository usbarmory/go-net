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
  * [lneto](https://github.com/soypat/lneto) networking library

The following packages provide compatible network devices:

  * [devcpu](https://pkg.go.dev/github.com/usbarmory/tamago/soc/microchip/devcpu): Microchip CPU port module
  * [enet](https://pkg.go.dev/github.com/usbarmory/tamago/soc/nxp/enet): NXP i.MX ENET Ethernet controller
  * [gvnic](https://pkg.go.dev/github.com/usbarmory/tamago/kvm/gvnic): Google Compute Engine Virtual Ethernet
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

// gnet interface with lneto stack
iface := gnet.Interface{
	Stack: NewLnetoStack(nil),
}

// initialize IP, MAC, Gateway
_ = iface.Init(nic, "10.0.0.1/24", "", "10.0.0.2")

// Go runtime hook
net.SocketFunc = iface.Stack.Socket
```

See the following projects for full integration examples of each supported network device:

* i.MX ENET/USB: [tamago-example](https://github.com/usbarmory/tamago-example/tree/master/network) (`mx6ullevk`, `imx8mpevk`, `usbarmory` targets)
* VirtIO:        [tamago-example](https://github.com/usbarmory/tamago-example/tree/master/network) (`cloud_hypervisor`, `firecracker`, `microvm` targets)
* gVNIC:         [tamago-sev-example](https://github.com/usbarmory/tamago-sev-example/blob/main/cmd/net_gvnic.go)
* UEFI:          [tamago-sev-example](https://github.com/usbarmory/tamago-sev-example/blob/main/cmd/net_gvnic.go), [go-boot](https://github.com/usbarmory/go-boot/blob/main/cmd/net.go)

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
