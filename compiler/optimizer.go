package compiler

import (
	"errors"

	"tinygo.org/x/go-llvm"
)

// Run the LLVM optimizer over the module.
// The inliner can be disabled (if necessary) by passing 0 to the inlinerThreshold.
func (c *Compiler) Optimize(optLevel, sizeLevel int, inlinerThreshold uint) error {
	builder := llvm.NewPassManagerBuilder()
	defer builder.Dispose()
	builder.SetOptLevel(optLevel)
	builder.SetSizeLevel(sizeLevel)
	if inlinerThreshold != 0 {
		builder.UseInlinerWithThreshold(inlinerThreshold)
	}
	builder.AddCoroutinePassesToExtensionPoints()

	if c.PanicStrategy == "trap" {
		c.replacePanicsWithTrap() // -panic=trap
	}

	// Run function passes for each function.
	funcPasses := llvm.NewFunctionPassManagerForModule(c.mod)
	defer funcPasses.Dispose()
	builder.PopulateFunc(funcPasses)
	funcPasses.InitializeFunc()
	for fn := c.mod.FirstFunction(); !fn.IsNil(); fn = llvm.NextFunction(fn) {
		funcPasses.RunFunc(fn)
	}
	funcPasses.FinalizeFunc()

	if optLevel > 0 {
		// Run some preparatory passes for the Go optimizer.
		goPasses := llvm.NewPassManager()
		defer goPasses.Dispose()
		goPasses.AddGlobalOptimizerPass()
		goPasses.AddGlobalDCEPass()
		goPasses.AddConstantPropagationPass()
		goPasses.AddAggressiveDCEPass()
		goPasses.AddFunctionAttrsPass()
		goPasses.Run(c.mod)

		// Run Go-specific optimization passes.
		c.OptimizeMaps()
		c.OptimizeStringToBytes()
		c.OptimizeAllocs()
		c.LowerInterfaces()
		c.LowerFuncValues()

		// After interfaces are lowered, there are many more opportunities for
		// interprocedural optimizations. To get them to work, function
		// attributes have to be updated first.
		goPasses.Run(c.mod)

		// Run TinyGo-specific interprocedural optimizations.
		c.OptimizeAllocs()
		c.OptimizeStringToBytes()

		// Lower runtime.isnil calls to regular nil comparisons.
		isnil := c.mod.NamedFunction("runtime.isnil")
		if !isnil.IsNil() {
			for _, use := range getUses(isnil) {
				c.builder.SetInsertPointBefore(use)
				ptr := use.Operand(0)
				if !ptr.IsABitCastInst().IsNil() {
					ptr = ptr.Operand(0)
				}
				nilptr := llvm.ConstPointerNull(ptr.Type())
				icmp := c.builder.CreateICmp(llvm.IntEQ, ptr, nilptr, "")
				use.ReplaceAllUsesWith(icmp)
				use.EraseFromParentAsInstruction()
			}
		}

		err := c.LowerGoroutines()
		if err != nil {
			return err
		}
	} else {
		// Must be run at any optimization level.
		c.LowerInterfaces()
		c.LowerFuncValues()
		err := c.LowerGoroutines()
		if err != nil {
			return err
		}
	}
	if err := c.Verify(); err != nil {
		return errors.New("optimizations caused a verification failure")
	}

	if sizeLevel >= 2 {
		// Set the "optsize" attribute to make slightly smaller binaries at the
		// cost of some performance.
		kind := llvm.AttributeKindID("optsize")
		attr := c.ctx.CreateEnumAttribute(kind, 0)
		for fn := c.mod.FirstFunction(); !fn.IsNil(); fn = llvm.NextFunction(fn) {
			fn.AddFunctionAttr(attr)
		}
	}

	// Run function passes again, because without it, llvm.coro.size.i32()
	// doesn't get lowered.
	for fn := c.mod.FirstFunction(); !fn.IsNil(); fn = llvm.NextFunction(fn) {
		funcPasses.RunFunc(fn)
	}
	funcPasses.FinalizeFunc()

	// Run module passes.
	modPasses := llvm.NewPassManager()
	defer modPasses.Dispose()
	builder.Populate(modPasses)
	modPasses.Run(c.mod)

	if c.gcIsPrecise() {
		c.addGlobalsBitmap()
		if err := c.Verify(); err != nil {
			return errors.New("GC pass caused a verification failure")
		}
	}

	return nil
}

// Replace panic calls with calls to llvm.trap, to reduce code size. This is the
// -panic=trap intrinsic.
func (c *Compiler) replacePanicsWithTrap() {
	trap := c.mod.NamedFunction("llvm.trap")
	for _, name := range []string{"runtime._panic", "runtime.runtimePanic"} {
		fn := c.mod.NamedFunction(name)
		if fn.IsNil() {
			continue
		}
		for _, use := range getUses(fn) {
			if use.IsACallInst().IsNil() || use.CalledValue() != fn {
				panic("expected use of a panic function to be a call")
			}
			c.builder.SetInsertPointBefore(use)
			c.builder.CreateCall(trap, nil, "")
		}
	}
}

// Eliminate created but not used maps.
//
// In the future, this should statically allocate created but never modified
// maps. This has not yet been implemented, however.
func (c *Compiler) OptimizeMaps() {
	hashmapMake := c.mod.NamedFunction("runtime.hashmapMake")
	if hashmapMake.IsNil() {
		// nothing to optimize
		return
	}

	hashmapBinarySet := c.mod.NamedFunction("runtime.hashmapBinarySet")
	hashmapStringSet := c.mod.NamedFunction("runtime.hashmapStringSet")

	for _, makeInst := range getUses(hashmapMake) {
		updateInsts := []llvm.Value{}
		unknownUses := false // are there any uses other than setting a value?

		for _, use := range getUses(makeInst) {
			if use := use.IsACallInst(); !use.IsNil() {
				switch use.CalledValue() {
				case hashmapBinarySet, hashmapStringSet:
					updateInsts = append(updateInsts, use)
				default:
					unknownUses = true
				}
			} else {
				unknownUses = true
			}
		}

		if !unknownUses {
			// This map can be entirely removed, as it is only created but never
			// used.
			for _, inst := range updateInsts {
				inst.EraseFromParentAsInstruction()
			}
			makeInst.EraseFromParentAsInstruction()
		}
	}
}

// Transform runtime.stringToBytes(...) calls into const []byte slices whenever
// possible. This optimizes the following pattern:
//     w.Write([]byte("foo"))
// where Write does not store to the slice.
func (c *Compiler) OptimizeStringToBytes() {
	stringToBytes := c.mod.NamedFunction("runtime.stringToBytes")
	if stringToBytes.IsNil() {
		// nothing to optimize
		return
	}

	for _, call := range getUses(stringToBytes) {
		strptr := call.Operand(0)
		strlen := call.Operand(1)

		// strptr is always constant because strings are always constant.

		convertedAllUses := true
		for _, use := range getUses(call) {
			nilValue := llvm.Value{}
			if use.IsAExtractValueInst() == nilValue {
				convertedAllUses = false
				continue
			}
			switch use.Type().TypeKind() {
			case llvm.IntegerTypeKind:
				// A length (len or cap). Propagate the length value.
				use.ReplaceAllUsesWith(strlen)
				use.EraseFromParentAsInstruction()
			case llvm.PointerTypeKind:
				// The string pointer itself.
				if !c.isReadOnly(use) {
					convertedAllUses = false
					continue
				}
				use.ReplaceAllUsesWith(strptr)
				use.EraseFromParentAsInstruction()
			default:
				// should not happen
				panic("unknown return type of runtime.stringToBytes: " + use.Type().String())
			}
		}
		if convertedAllUses {
			// Call to runtime.stringToBytes can be eliminated: both the input
			// and the output is constant.
			call.EraseFromParentAsInstruction()
		}
	}
}

// Basic escape analysis: translate runtime.alloc calls into alloca
// instructions.
func (c *Compiler) OptimizeAllocs() {
	allocator := c.mod.NamedFunction("runtime.alloc")
	if allocator.IsNil() {
		// nothing to optimize
		return
	}

	heapallocs := getUses(allocator)
	for _, heapalloc := range heapallocs {
		nilValue := llvm.Value{}
		if heapalloc.Operand(0).IsAConstant() == nilValue {
			// Do not allocate variable length arrays on the stack.
			continue
		}
		size := heapalloc.Operand(0).ZExtValue()
		if size > 256 {
			// The maximum value for a stack allocation.
			// TODO: tune this, this is just a random value.
			continue
		}

		// In general the pattern is:
		//     %0 = call i8* @runtime.alloc(i32 %size)
		//     %1 = bitcast i8* %0 to type*
		//     (use %1 only)
		// But the bitcast might sometimes be dropped when allocating an *i8.
		// The 'bitcast' variable below is thus usually a bitcast of the
		// heapalloc but not always.
		bitcast := heapalloc // instruction that creates the value
		if uses := getUses(heapalloc); len(uses) == 1 && uses[0].IsABitCastInst() != nilValue {
			// getting only bitcast use
			bitcast = uses[0]
		}
		if !c.doesEscape(bitcast) {
			// Insert alloca in the entry block. Do it here so that mem2reg can
			// promote it to a SSA value.
			fn := bitcast.InstructionParent().Parent()
			c.builder.SetInsertPointBefore(fn.EntryBasicBlock().FirstInstruction())
			alignment := c.targetData.ABITypeAlignment(c.i8ptrType)
			sizeInWords := (size + uint64(alignment) - 1) / uint64(alignment)
			allocaType := llvm.ArrayType(c.ctx.IntType(alignment*8), int(sizeInWords))
			alloca := c.builder.CreateAlloca(allocaType, "stackalloc.alloca")
			zero := c.getZeroValue(alloca.Type().ElementType())
			c.builder.CreateStore(zero, alloca)
			stackalloc := c.builder.CreateBitCast(alloca, bitcast.Type(), "stackalloc")
			bitcast.ReplaceAllUsesWith(stackalloc)
			if heapalloc != bitcast {
				bitcast.EraseFromParentAsInstruction()
			}
			heapalloc.EraseFromParentAsInstruction()
		}
	}
}

// Very basic escape analysis.
func (c *Compiler) doesEscape(value llvm.Value) bool {
	uses := getUses(value)
	for _, use := range uses {
		nilValue := llvm.Value{}
		if use.IsAGetElementPtrInst() != nilValue {
			if c.doesEscape(use) {
				return true
			}
		} else if use.IsABitCastInst() != nilValue {
			// A bitcast escapes if the casted-to value escapes.
			if c.doesEscape(use) {
				return true
			}
		} else if use.IsALoadInst() != nilValue {
			// Load does not escape.
		} else if use.IsAStoreInst() != nilValue {
			// Store only escapes when the value is stored to, not when the
			// value is stored into another value.
			if use.Operand(0) == value {
				return true
			}
		} else if use.IsACallInst() != nilValue {
			if !c.hasFlag(use, value, "nocapture") {
				return true
			}
		} else if use.IsAICmpInst() != nilValue {
			// Comparing pointers don't let the pointer escape.
			// This is often a compiler-inserted nil check.
		} else {
			// Unknown instruction, might escape.
			return true
		}
	}

	// does not escape
	return false
}

// Check whether the given value (which is of pointer type) is never stored to.
func (c *Compiler) isReadOnly(value llvm.Value) bool {
	uses := getUses(value)
	for _, use := range uses {
		nilValue := llvm.Value{}
		if use.IsAGetElementPtrInst() != nilValue {
			if !c.isReadOnly(use) {
				return false
			}
		} else if use.IsACallInst() != nilValue {
			if !c.hasFlag(use, value, "readonly") {
				return false
			}
		} else {
			// Unknown instruction, might not be readonly.
			return false
		}
	}
	return true
}

// Check whether all uses of this param as parameter to the call have the given
// flag. In most cases, there will only be one use but a function could take the
// same parameter twice, in which case both must have the flag.
// A flag can be any enum flag, like "readonly".
func (c *Compiler) hasFlag(call, param llvm.Value, kind string) bool {
	fn := call.CalledValue()
	nilValue := llvm.Value{}
	if fn.IsAFunction() == nilValue {
		// This is not a function but something else, like a function pointer.
		return false
	}
	kindID := llvm.AttributeKindID(kind)
	for i := 0; i < fn.ParamsCount(); i++ {
		if call.Operand(i) != param {
			// This is not the parameter we're checking.
			continue
		}
		index := i + 1 // param attributes start at 1
		attr := fn.GetEnumAttributeAtIndex(index, kindID)
		nilAttribute := llvm.Attribute{}
		if attr == nilAttribute {
			// At least one parameter doesn't have the flag (there may be
			// multiple).
			return false
		}
	}
	return true
}
