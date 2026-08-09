// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

//go:build nodefaultstack

package gnet

// newDefaultStack under the "nodefaultstack" tag, where gvisor.go is excluded
// from the build. [Interface.Init] returns an error when Stack is nil.
func newDefaultStack() Stack { return nil }
