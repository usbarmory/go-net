// Copyright (c) The go-net authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

//go:build !gnet_lneto

package gnet

func newDefaultStack() *GVisorStack {
	return NewGVisorStack(1)
}
