// Copyright 2022 The GoPlus Authors (goplus.org). All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"

	"golang.org/x/tools/gopls/goxls/lsview"
)

const (
	gopls = "gopls.origin"
	goxls = "goxls"
)

func main() {
	lsview.Main(gopls, goxls, os.Args[1:]...)
}
