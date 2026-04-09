Bare metal Go TCP/IP connectivity
=================================

This Go package implements TCP/IP connectivity through a generic network
interface to be used with `GOOS=tamago` as supported by the
[TamaGo](https://github.com/usbarmory/tamago) framework for bare metal Go.

The package supports TCP/IP networking through gVisor (`go` branch)
[tcpip](https://pkg.go.dev/gvisor.dev/gvisor/pkg/tcpip)
stack pure Go implementation.

The interface TCP/IP stack can be attached to the Go runtime by setting
`net.SocketFunc` to the interface `Socket` function:

```go
// TamaGo UEFI Simple Network interface
nic, _ := &x64.UEFI.Boot.GetNetwork{}

// Create IP/TCP networking stack.
netstack := gnet.NewDefaultStack()

// Bridge NIC with networking stack with gnet interface type
// and give interface IP, MAC, Gateway.
var iface gnet.Interface
_ = iface.Init(nic, netstack, addr, mac, gateway)
go iface.StartRx()

// Go runtime hook
net.SocketFunc = netstack.Socket
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
