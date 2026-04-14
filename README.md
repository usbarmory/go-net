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

Support for the following network devices is provided:

  * [uefi](https://github.com/usbarmory/go-boot/blob/main/uefi/net.go) through [go-boot](https://github.com/usbarmory/go-boot/) [Simple Network driver](https://pkg.go.dev/github.com/usbarmory/go-bootuefi#SimpleNetwork)
  * [virtio](https://github.com/usbarmory/go-net/blob/main/virtio) network device through tamago [VirtIO driver](https://pkg.go.dev/github.com/usbarmory/tamago/kvm/virtio)

Example
=======

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

See [go-boot](https://github.com/usbarmory/go-boot/blob/development/cmd/net.go)
for a full integration example with the UEFI Simple Network Protocol.

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
