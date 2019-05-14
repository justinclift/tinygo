// +build wasm

package runtime

import (
	"unsafe"
)

const GOARCH = "wasm"

// The bitness of the CPU (e.g. 8, 32, 64).
const TargetBits = 32

//go:extern __heap_base
var heapStartSymbol unsafe.Pointer

//go:extern _heap_end
//var heapEndSymbol unsafe.Pointer

//go:extern _globals_start
var globalsStartSymbol unsafe.Pointer

//go:extern _globals_end
var globalsEndSymbol unsafe.Pointer

//go:extern _stack_top
var stackTopSymbol unsafe.Pointer

var (
	heapStart = uintptr(unsafe.Pointer(&heapStartSymbol))
	heapEnd   = (heapStart + wasmPageSize - 1) &^ (wasmPageSize - 1) // conservative guess: one page of heap memory
	globalsStart = uintptr(unsafe.Pointer(&globalsStartSymbol))
	globalsEnd   = uintptr(unsafe.Pointer(&globalsEndSymbol))
	stackTop     = uintptr(unsafe.Pointer(&stackTopSymbol))
)

const wasmPageSize = 64 * 1024

// Align on word boundary.
func align(ptr uintptr) uintptr {
	return (ptr + 3) &^ 3
}
