// +build !byollvm

package cgo

/*
#cgo linux  CFLAGS: -I/opt/llvm8-wasm/include
#cgo darwin CFLAGS: -I/usr/local/opt/llvm/include
#cgo linux  LDFLAGS: -L/opt/llvm8-wasm/lib -lclang
#cgo darwin LDFLAGS: -L/usr/local/opt/llvm/lib -lclang -lffi
*/
import "C"
