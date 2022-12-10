package compiler

// This file transforms interface-related instructions (*ssa.MakeInterface,
// *ssa.TypeAssert, calls on interface types) to an intermediate IR form, to be
// lowered to the final form by the interface lowering pass. See
// interface-lowering.go for more details.

import (
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
	"tinygo.org/x/go-llvm"
)

// Type kinds for basic types.
// They must match the constants for the Kind type in src/reflect/type.go.
var basicTypes = [...]uint8{
	types.Bool:          1,
	types.Int:           2,
	types.Int8:          3,
	types.Int16:         4,
	types.Int32:         5,
	types.Int64:         6,
	types.Uint:          7,
	types.Uint8:         8,
	types.Uint16:        9,
	types.Uint32:        10,
	types.Uint64:        11,
	types.Uintptr:       12,
	types.Float32:       13,
	types.Float64:       14,
	types.Complex64:     15,
	types.Complex128:    16,
	types.String:        17,
	types.UnsafePointer: 18,

	// named types are as follows:
	// chan:      19
	// interface: 20
	// pointer:   21
	// slice:     22
	// array:     23
	// signature: 24
	// map:       25
	// struct:    26
}

// createMakeInterface emits the LLVM IR for the *ssa.MakeInterface instruction.
// It tries to put the type in the interface value, but if that's not possible,
// it will do an allocation of the right size and put that in the interface
// value field.
//
// An interface value is a {typecode, value} tuple named runtime._interface.
func (b *builder) createMakeInterface(val llvm.Value, typ types.Type, pos token.Pos) llvm.Value {
	itfValue := b.emitPointerPack([]llvm.Value{val})
	itfType := b.getTypeCode(typ)
	itf := llvm.Undef(b.getLLVMRuntimeType("_interface"))
	itf = b.CreateInsertValue(itf, itfType, 0, "")
	itf = b.CreateInsertValue(itf, itfValue, 1, "")
	return itf
}

// extractValueFromInterface extract the value from an interface value
// (runtime._interface) under the assumption that it is of the type given in
// llvmType. The behavior is undefied if the interface is nil or llvmType
// doesn't match the underlying type of the interface.
func (b *builder) extractValueFromInterface(itf llvm.Value, llvmType llvm.Type) llvm.Value {
	valuePtr := b.CreateExtractValue(itf, 1, "typeassert.value.ptr")
	return b.emitPointerUnpack(valuePtr, []llvm.Type{llvmType})[0]
}

// getTypeCode returns a reference to a type code.
// A type code is a pointer to a constant global that describes the type.
// This function returns a pointer to the 'kind' field (which might not be the
// first field in the struct).
func (c *compilerContext) getTypeCode(typ types.Type) llvm.Value {
	ms := c.program.MethodSets.MethodSet(typ)
	hasMethodSet := ms.Len() != 0
	if _, ok := typ.Underlying().(*types.Interface); ok {
		hasMethodSet = false
	}
	globalName := "reflect/types.type:" + getTypeCodeName(typ)
	global := c.mod.NamedGlobal(globalName)
	if global.IsNil() {
		var typeFields []llvm.Value
		// Define the type fields. These must match the structs in
		// src/reflect/type.go (ptrType, arrayType, etc):
		//   basic:
		//     kind, ptrTo
		//   named:
		//     kind, ptrTo, underlying
		//   chan, slice:
		//     kind, ptrTo, elementType
		//   pointer:
		//     kind, elementType
		//   array:
		//     kind, ptrTo, elementType, length
		//   map:
		//     kind, ptrTo, [todo: elemType, keyType]
		//   struct:
		//     kind, numFields, ptrTo, fields...
		//   interface:
		//     kind, ptrTo, numMethods, methods...
		//   signature:
		//     kind, [todo]
		typeFieldTypes := []*types.Var{
			types.NewVar(token.NoPos, nil, "kind", types.Typ[types.Int8]),
		}
		switch typ := typ.(type) {
		case *types.Basic:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
			)
		case *types.Named:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
				types.NewVar(token.NoPos, nil, "underlying", types.Typ[types.UnsafePointer]),
			)
		case *types.Chan, *types.Slice:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
				types.NewVar(token.NoPos, nil, "elementType", types.Typ[types.UnsafePointer]),
			)
		case *types.Pointer:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "elementType", types.Typ[types.UnsafePointer]),
			)
		case *types.Array:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
				types.NewVar(token.NoPos, nil, "elementType", types.Typ[types.UnsafePointer]),
				types.NewVar(token.NoPos, nil, "length", types.Typ[types.Uintptr]),
			)
		case *types.Map:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
			)
		case *types.Struct:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "numFields", types.Typ[types.Uint16]),
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
				types.NewVar(token.NoPos, nil, "fields", types.NewArray(c.getRuntimeType("structField"), int64(typ.NumFields()))),
			)
		case *types.Interface:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
			)
			// TODO: methods
		case *types.Signature:
			typeFieldTypes = append(typeFieldTypes,
				types.NewVar(token.NoPos, nil, "ptrTo", types.Typ[types.UnsafePointer]),
			)
			// TODO: signature params and return values
		}
		if hasMethodSet {
			typeFieldTypes = append([]*types.Var{
				types.NewVar(token.NoPos, nil, "methodSet", types.Typ[types.UnsafePointer]),
			}, typeFieldTypes...)
		}
		globalType := types.NewStruct(typeFieldTypes, nil)
		global = llvm.AddGlobal(c.mod, c.getLLVMType(globalType), globalName)
		metabyte := getTypeKind(typ)
		switch typ := typ.(type) {
		case *types.Basic:
			typeFields = []llvm.Value{c.getTypeCode(types.NewPointer(typ))}
		case *types.Named:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Underlying()),      // underlying
			}
			metabyte |= 1 << 5 // "named" flag
		case *types.Chan:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Elem()),            // elementType
			}
		case *types.Slice:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Elem()),            // elementType
			}
		case *types.Pointer:
			typeFields = []llvm.Value{c.getTypeCode(typ.Elem())}
		case *types.Array:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)),                   // ptrTo
				c.getTypeCode(typ.Elem()),                              // elementType
				llvm.ConstInt(c.uintptrType, uint64(typ.Len()), false), // length
			}
		case *types.Map:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
			}
		case *types.Struct:
			typeFields = []llvm.Value{
				llvm.ConstInt(c.ctx.Int16Type(), uint64(typ.NumFields()), false), // numFields
				c.getTypeCode(types.NewPointer(typ)),                             // ptrTo
			}
			structFieldType := c.getLLVMRuntimeType("structField")
			var fields []llvm.Value
			for i := 0; i < typ.NumFields(); i++ {
				field := typ.Field(i)
				var flags uint8
				if field.Anonymous() {
					flags |= 1
				}
				if typ.Tag(i) != "" {
					flags |= 2
				}
				if token.IsExported(field.Name()) {
					flags |= 4
				}
				data := string(flags) + field.Name()
				if typ.Tag(i) != "" {
					data += "\x00" + typ.Tag(i)
				}
				dataInitializer := c.ctx.ConstString(data, true)
				dataGlobal := llvm.AddGlobal(c.mod, dataInitializer.Type(), globalName+"."+field.Name())
				dataGlobal.SetInitializer(dataInitializer)
				dataGlobal.SetAlignment(1)
				dataGlobal.SetUnnamedAddr(true)
				dataGlobal.SetLinkage(llvm.InternalLinkage)
				dataGlobal.SetGlobalConstant(true)
				fieldType := c.getTypeCode(field.Type())
				fields = append(fields, llvm.ConstNamedStruct(structFieldType, []llvm.Value{
					fieldType,
					llvm.ConstGEP(dataGlobal.GlobalValueType(), dataGlobal, []llvm.Value{
						llvm.ConstInt(c.ctx.Int32Type(), 0, false),
						llvm.ConstInt(c.ctx.Int32Type(), 0, false),
					}),
				}))
			}
			typeFields = append(typeFields, llvm.ConstArray(structFieldType, fields))
		case *types.Interface:
			typeFields = []llvm.Value{c.getTypeCode(types.NewPointer(typ))}
			// TODO: methods
		case *types.Signature:
			typeFields = []llvm.Value{c.getTypeCode(types.NewPointer(typ))}
			// TODO: params, return values, etc
		}
		// Prepend metadata byte.
		typeFields = append([]llvm.Value{
			llvm.ConstInt(c.ctx.Int8Type(), uint64(metabyte), false),
		}, typeFields...)
		if hasMethodSet {
			typeFields = append([]llvm.Value{
				llvm.ConstBitCast(c.getTypeMethodSet(typ), c.i8ptrType),
			}, typeFields...)
		}
		alignment := c.targetData.TypeAllocSize(c.i8ptrType)
		globalValue := c.ctx.ConstStruct(typeFields, false)
		global.SetInitializer(globalValue)
		global.SetLinkage(llvm.LinkOnceODRLinkage)
		global.SetGlobalConstant(true)
		global.SetAlignment(int(alignment))
		if c.Debug {
			file := c.getDIFile("<Go type>")
			diglobal := c.dibuilder.CreateGlobalVariableExpression(file, llvm.DIGlobalVariableExpression{
				Name:        "type " + typ.String(),
				File:        file,
				Line:        1,
				Type:        c.getDIType(globalType),
				LocalToUnit: false,
				Expr:        c.dibuilder.CreateExpression(nil),
				AlignInBits: uint32(alignment * 8),
			})
			global.AddMetadata(0, diglobal)
		}
	}
	offset := uint64(0)
	if hasMethodSet {
		// The GEP has to point to the 'kind' field, which may not be at the
		// start of the struct.
		offset = 1
	}
	return llvm.ConstGEP(global.GlobalValueType(), global, []llvm.Value{
		llvm.ConstInt(llvm.Int32Type(), 0, false),
		llvm.ConstInt(llvm.Int32Type(), offset, false),
	})
}

// getTypeKind returns the type kind for the given type, as defined by
// reflect.Kind.
func getTypeKind(t types.Type) uint8 {
	switch t := t.Underlying().(type) {
	case *types.Basic:
		return basicTypes[t.Kind()]
	case *types.Chan:
		return 19
	case *types.Interface:
		return 20
	case *types.Pointer:
		return 21
	case *types.Slice:
		return 22
	case *types.Array:
		return 23
	case *types.Signature:
		return 24
	case *types.Map:
		return 25
	case *types.Struct:
		return 26
	default:
		panic("unknown type")
	}
}

var basicTypeNames = [...]string{
	types.Bool:          "bool",
	types.Int:           "int",
	types.Int8:          "int8",
	types.Int16:         "int16",
	types.Int32:         "int32",
	types.Int64:         "int64",
	types.Uint:          "uint",
	types.Uint8:         "uint8",
	types.Uint16:        "uint16",
	types.Uint32:        "uint32",
	types.Uint64:        "uint64",
	types.Uintptr:       "uintptr",
	types.Float32:       "float32",
	types.Float64:       "float64",
	types.Complex64:     "complex64",
	types.Complex128:    "complex128",
	types.String:        "string",
	types.UnsafePointer: "unsafe.Pointer",
}

// getTypeCodeName returns a name for this type that can be used in the
// interface lowering pass to assign type codes as expected by the reflect
// package. See getTypeCodeNum.
func getTypeCodeName(t types.Type) string {
	switch t := t.(type) {
	case *types.Named:
		return "named:" + t.String()
	case *types.Array:
		return "array:" + strconv.FormatInt(t.Len(), 10) + ":" + getTypeCodeName(t.Elem())
	case *types.Basic:
		return "basic:" + basicTypeNames[t.Kind()]
	case *types.Chan:
		return "chan:" + getTypeCodeName(t.Elem())
	case *types.Interface:
		methods := make([]string, t.NumMethods())
		for i := 0; i < t.NumMethods(); i++ {
			name := t.Method(i).Name()
			if !token.IsExported(name) {
				name = t.Method(i).Pkg().Path() + "." + name
			}
			methods[i] = name + ":" + getTypeCodeName(t.Method(i).Type())
		}
		return "interface:" + "{" + strings.Join(methods, ",") + "}"
	case *types.Map:
		keyType := getTypeCodeName(t.Key())
		elemType := getTypeCodeName(t.Elem())
		return "map:" + "{" + keyType + "," + elemType + "}"
	case *types.Pointer:
		return "pointer:" + getTypeCodeName(t.Elem())
	case *types.Signature:
		params := make([]string, t.Params().Len())
		for i := 0; i < t.Params().Len(); i++ {
			params[i] = getTypeCodeName(t.Params().At(i).Type())
		}
		results := make([]string, t.Results().Len())
		for i := 0; i < t.Results().Len(); i++ {
			results[i] = getTypeCodeName(t.Results().At(i).Type())
		}
		return "func:" + "{" + strings.Join(params, ",") + "}{" + strings.Join(results, ",") + "}"
	case *types.Slice:
		return "slice:" + getTypeCodeName(t.Elem())
	case *types.Struct:
		elems := make([]string, t.NumFields())
		for i := 0; i < t.NumFields(); i++ {
			embedded := ""
			if t.Field(i).Embedded() {
				embedded = "#"
			}
			elems[i] = embedded + t.Field(i).Name() + ":" + getTypeCodeName(t.Field(i).Type())
			if t.Tag(i) != "" {
				elems[i] += "`" + t.Tag(i) + "`"
			}
		}
		return "struct:" + "{" + strings.Join(elems, ",") + "}"
	default:
		panic("unknown type: " + t.String())
	}
}

// getTypeMethodSet returns a reference (GEP) to a global method set. This
// method set should be unreferenced after the interface lowering pass.
func (c *compilerContext) getTypeMethodSet(typ types.Type) llvm.Value {
	globalName := typ.String() + "$methodset"
	global := c.mod.NamedGlobal(globalName)
	if global.IsNil() {
		ms := c.program.MethodSets.MethodSet(typ)

		// Create method set.
		var signatures, wrappers []llvm.Value
		for i := 0; i < ms.Len(); i++ {
			method := ms.At(i)
			signatureGlobal := c.getMethodSignature(method.Obj().(*types.Func))
			signatures = append(signatures, signatureGlobal)
			fn := c.program.MethodValue(method)
			llvmFnType, llvmFn := c.getFunction(fn)
			if llvmFn.IsNil() {
				// compiler error, so panic
				panic("cannot find function: " + c.getFunctionInfo(fn).linkName)
			}
			wrapper := c.getInterfaceInvokeWrapper(fn, llvmFnType, llvmFn)
			wrappers = append(wrappers, wrapper)
		}

		// Construct global value.
		globalValue := c.ctx.ConstStruct([]llvm.Value{
			llvm.ConstInt(c.uintptrType, uint64(ms.Len()), false),
			llvm.ConstArray(c.i8ptrType, signatures),
			c.ctx.ConstStruct(wrappers, false),
		}, false)
		global = llvm.AddGlobal(c.mod, globalValue.Type(), globalName)
		global.SetInitializer(globalValue)
		global.SetGlobalConstant(true)
		global.SetUnnamedAddr(true)
		global.SetLinkage(llvm.LinkOnceODRLinkage)
	}
	return global
}

// getMethodSignatureName returns a unique name (that can be used as the name of
// a global) for the given method.
func (c *compilerContext) getMethodSignatureName(method *types.Func) string {
	signature := methodSignature(method)
	var globalName string
	if token.IsExported(method.Name()) {
		globalName = "reflect/methods." + signature
	} else {
		globalName = method.Type().(*types.Signature).Recv().Pkg().Path() + ".$methods." + signature
	}
	return globalName
}

// getMethodSignature returns a global variable which is a reference to an
// external *i8 indicating the indicating the signature of this method. It is
// used during the interface lowering pass.
func (c *compilerContext) getMethodSignature(method *types.Func) llvm.Value {
	globalName := c.getMethodSignatureName(method)
	signatureGlobal := c.mod.NamedGlobal(globalName)
	if signatureGlobal.IsNil() {
		// TODO: put something useful in these globals, such as the method
		// signature. Useful to one day implement reflect.Value.Method(n).
		signatureGlobal = llvm.AddGlobal(c.mod, c.ctx.Int8Type(), globalName)
		signatureGlobal.SetInitializer(llvm.ConstInt(c.ctx.Int8Type(), 0, false))
		signatureGlobal.SetLinkage(llvm.LinkOnceODRLinkage)
		signatureGlobal.SetGlobalConstant(true)
		signatureGlobal.SetAlignment(1)
	}
	return signatureGlobal
}

// createTypeAssert will emit the code for a typeassert, used in if statements
// and in type switches (Go SSA does not have type switches, only if/else
// chains). Note that even though the Go SSA does not contain type switches,
// LLVM will recognize the pattern and make it a real switch in many cases.
//
// Type asserts on concrete types are trivial: just compare type numbers. Type
// asserts on interfaces are more difficult, see the comments in the function.
func (b *builder) createTypeAssert(expr *ssa.TypeAssert) llvm.Value {
	itf := b.getValue(expr.X)
	assertedType := b.getLLVMType(expr.AssertedType)

	actualTypeNum := b.CreateExtractValue(itf, 0, "interface.type")
	commaOk := llvm.Value{}
	if _, ok := expr.AssertedType.Underlying().(*types.Interface); ok {
		// Type assert on interface type.
		// This is a call to an interface type assert function.
		// The interface lowering pass will define this function by filling it
		// with a type switch over all concrete types that implement this
		// interface, and returning whether it's one of the matched types.
		// This is very different from how interface asserts are implemented in
		// the main Go compiler, where the runtime checks whether the type
		// implements each method of the interface. See:
		// https://research.swtch.com/interfaces
		fn := b.getInterfaceImplementsFunc(expr.AssertedType)
		commaOk = b.CreateCall(fn.GlobalValueType(), fn, []llvm.Value{actualTypeNum}, "")

	} else {
		globalName := "reflect/types.typeid:" + getTypeCodeName(expr.AssertedType)
		assertedTypeCodeGlobal := b.mod.NamedGlobal(globalName)
		if assertedTypeCodeGlobal.IsNil() {
			// Create a new typecode global.
			assertedTypeCodeGlobal = llvm.AddGlobal(b.mod, b.ctx.Int8Type(), globalName)
			assertedTypeCodeGlobal.SetGlobalConstant(true)
		}
		// Type assert on concrete type.
		// Call runtime.typeAssert, which will be lowered to a simple icmp or
		// const false in the interface lowering pass.
		commaOk = b.createRuntimeCall("typeAssert", []llvm.Value{actualTypeNum, assertedTypeCodeGlobal}, "typecode")
	}

	// Add 2 new basic blocks (that should get optimized away): one for the
	// 'ok' case and one for all instructions following this type assert.
	// This is necessary because we need to insert the casted value or the
	// nil value based on whether the assert was successful. Casting before
	// this check tells LLVM that it can use this value and may
	// speculatively dereference pointers before the check. This can lead to
	// a miscompilation resulting in a segfault at runtime.
	// Additionally, this is even required by the Go spec: a failed
	// typeassert should return a zero value, not an incorrectly casted
	// value.

	prevBlock := b.GetInsertBlock()
	okBlock := b.insertBasicBlock("typeassert.ok")
	nextBlock := b.insertBasicBlock("typeassert.next")
	b.blockExits[b.currentBlock] = nextBlock // adjust outgoing block for phi nodes
	b.CreateCondBr(commaOk, okBlock, nextBlock)

	// Retrieve the value from the interface if the type assert was
	// successful.
	b.SetInsertPointAtEnd(okBlock)
	var valueOk llvm.Value
	if _, ok := expr.AssertedType.Underlying().(*types.Interface); ok {
		// Type assert on interface type. Easy: just return the same
		// interface value.
		valueOk = itf
	} else {
		// Type assert on concrete type. Extract the underlying type from
		// the interface (but only after checking it matches).
		valueOk = b.extractValueFromInterface(itf, assertedType)
	}
	b.CreateBr(nextBlock)

	// Continue after the if statement.
	b.SetInsertPointAtEnd(nextBlock)
	phi := b.CreatePHI(assertedType, "typeassert.value")
	phi.AddIncoming([]llvm.Value{llvm.ConstNull(assertedType), valueOk}, []llvm.BasicBlock{prevBlock, okBlock})

	if expr.CommaOk {
		tuple := b.ctx.ConstStruct([]llvm.Value{llvm.Undef(assertedType), llvm.Undef(b.ctx.Int1Type())}, false) // create empty tuple
		tuple = b.CreateInsertValue(tuple, phi, 0, "")                                                          // insert value
		tuple = b.CreateInsertValue(tuple, commaOk, 1, "")                                                      // insert 'comma ok' boolean
		return tuple
	} else {
		// This is kind of dirty as the branch above becomes mostly useless,
		// but hopefully this gets optimized away.
		b.createRuntimeCall("interfaceTypeAssert", []llvm.Value{commaOk}, "")
		return phi
	}
}

// getMethodsString returns a string to be used in the "tinygo-methods" string
// attribute for interface functions.
func (c *compilerContext) getMethodsString(itf *types.Interface) string {
	methods := make([]string, itf.NumMethods())
	for i := range methods {
		methods[i] = c.getMethodSignatureName(itf.Method(i))
	}
	return strings.Join(methods, "; ")
}

// getInterfaceImplementsfunc returns a declared function that works as a type
// switch. The interface lowering pass will define this function.
func (c *compilerContext) getInterfaceImplementsFunc(assertedType types.Type) llvm.Value {
	fnName := getTypeCodeName(assertedType.Underlying()) + ".$typeassert"
	llvmFn := c.mod.NamedFunction(fnName)
	if llvmFn.IsNil() {
		llvmFnType := llvm.FunctionType(c.ctx.Int1Type(), []llvm.Type{c.i8ptrType}, false)
		llvmFn = llvm.AddFunction(c.mod, fnName, llvmFnType)
		c.addStandardDeclaredAttributes(llvmFn)
		methods := c.getMethodsString(assertedType.Underlying().(*types.Interface))
		llvmFn.AddFunctionAttr(c.ctx.CreateStringAttribute("tinygo-methods", methods))
	}
	return llvmFn
}

// getInvokeFunction returns the thunk to call the given interface method. The
// thunk is declared, not defined: it will be defined by the interface lowering
// pass.
func (c *compilerContext) getInvokeFunction(instr *ssa.CallCommon) llvm.Value {
	fnName := getTypeCodeName(instr.Value.Type().Underlying()) + "." + instr.Method.Name() + "$invoke"
	llvmFn := c.mod.NamedFunction(fnName)
	if llvmFn.IsNil() {
		sig := instr.Method.Type().(*types.Signature)
		var paramTuple []*types.Var
		for i := 0; i < sig.Params().Len(); i++ {
			paramTuple = append(paramTuple, sig.Params().At(i))
		}
		paramTuple = append(paramTuple, types.NewVar(token.NoPos, nil, "$typecode", types.Typ[types.UnsafePointer]))
		llvmFnType := c.getRawFuncType(types.NewSignature(sig.Recv(), types.NewTuple(paramTuple...), sig.Results(), false))
		llvmFn = llvm.AddFunction(c.mod, fnName, llvmFnType)
		c.addStandardDeclaredAttributes(llvmFn)
		llvmFn.AddFunctionAttr(c.ctx.CreateStringAttribute("tinygo-invoke", c.getMethodSignatureName(instr.Method)))
		methods := c.getMethodsString(instr.Value.Type().Underlying().(*types.Interface))
		llvmFn.AddFunctionAttr(c.ctx.CreateStringAttribute("tinygo-methods", methods))
	}
	return llvmFn
}

// getInterfaceInvokeWrapper returns a wrapper for the given method so it can be
// invoked from an interface. The wrapper takes in a pointer to the underlying
// value, dereferences or unpacks it if necessary, and calls the real method.
// If the method to wrap has a pointer receiver, no wrapping is necessary and
// the function is returned directly.
func (c *compilerContext) getInterfaceInvokeWrapper(fn *ssa.Function, llvmFnType llvm.Type, llvmFn llvm.Value) llvm.Value {
	wrapperName := llvmFn.Name() + "$invoke"
	wrapper := c.mod.NamedFunction(wrapperName)
	if !wrapper.IsNil() {
		// Wrapper already created. Return it directly.
		return wrapper
	}

	// Get the expanded receiver type.
	receiverType := c.getLLVMType(fn.Signature.Recv().Type())
	var expandedReceiverType []llvm.Type
	for _, info := range c.expandFormalParamType(receiverType, "", nil) {
		expandedReceiverType = append(expandedReceiverType, info.llvmType)
	}

	// Does this method even need any wrapping?
	if len(expandedReceiverType) == 1 && receiverType.TypeKind() == llvm.PointerTypeKind {
		// Nothing to wrap.
		// Casting a function signature to a different signature and calling it
		// with a receiver pointer bitcasted to *i8 (as done in calls on an
		// interface) is hopefully a safe (defined) operation.
		return llvmFn
	}

	// create wrapper function
	paramTypes := append([]llvm.Type{c.i8ptrType}, llvmFnType.ParamTypes()[len(expandedReceiverType):]...)
	wrapFnType := llvm.FunctionType(llvmFnType.ReturnType(), paramTypes, false)
	wrapper = llvm.AddFunction(c.mod, wrapperName, wrapFnType)
	c.addStandardAttributes(wrapper)

	wrapper.SetLinkage(llvm.LinkOnceODRLinkage)
	wrapper.SetUnnamedAddr(true)

	// Create a new builder just to create this wrapper.
	b := builder{
		compilerContext: c,
		Builder:         c.ctx.NewBuilder(),
	}
	defer b.Builder.Dispose()

	// add debug info if needed
	if c.Debug {
		pos := c.program.Fset.Position(fn.Pos())
		difunc := c.attachDebugInfoRaw(fn, wrapper, "$invoke", pos.Filename, pos.Line)
		b.SetCurrentDebugLocation(uint(pos.Line), uint(pos.Column), difunc, llvm.Metadata{})
	}

	// set up IR builder
	block := b.ctx.AddBasicBlock(wrapper, "entry")
	b.SetInsertPointAtEnd(block)

	receiverValue := b.emitPointerUnpack(wrapper.Param(0), []llvm.Type{receiverType})[0]
	params := append(b.expandFormalParam(receiverValue), wrapper.Params()[1:]...)
	if llvmFnType.ReturnType().TypeKind() == llvm.VoidTypeKind {
		b.CreateCall(llvmFnType, llvmFn, params, "")
		b.CreateRetVoid()
	} else {
		ret := b.CreateCall(llvmFnType, llvmFn, params, "ret")
		b.CreateRet(ret)
	}

	return wrapper
}

// methodSignature creates a readable version of a method signature (including
// the function name, excluding the receiver name). This string is used
// internally to match interfaces and to call the correct method on an
// interface. Examples:
//
//	String() string
//	Read([]byte) (int, error)
func methodSignature(method *types.Func) string {
	return method.Name() + signature(method.Type().(*types.Signature))
}

// Make a readable version of a function (pointer) signature.
// Examples:
//
//	() string
//	(string, int) (int, error)
func signature(sig *types.Signature) string {
	s := ""
	if sig.Params().Len() == 0 {
		s += "()"
	} else {
		s += "("
		for i := 0; i < sig.Params().Len(); i++ {
			if i > 0 {
				s += ", "
			}
			s += typestring(sig.Params().At(i).Type())
		}
		s += ")"
	}
	if sig.Results().Len() == 0 {
		// keep as-is
	} else if sig.Results().Len() == 1 {
		s += " " + typestring(sig.Results().At(0).Type())
	} else {
		s += " ("
		for i := 0; i < sig.Results().Len(); i++ {
			if i > 0 {
				s += ", "
			}
			s += typestring(sig.Results().At(i).Type())
		}
		s += ")"
	}
	return s
}

// typestring returns a stable (human-readable) type string for the given type
// that can be used for interface equality checks. It is almost (but not
// exactly) the same as calling t.String(). The main difference is some
// normalization around `byte` vs `uint8` for example.
func typestring(t types.Type) string {
	// See: https://github.com/golang/go/blob/master/src/go/types/typestring.go
	switch t := t.(type) {
	case *types.Array:
		return "[" + strconv.FormatInt(t.Len(), 10) + "]" + typestring(t.Elem())
	case *types.Basic:
		return basicTypeNames[t.Kind()]
	case *types.Chan:
		switch t.Dir() {
		case types.SendRecv:
			return "chan (" + typestring(t.Elem()) + ")"
		case types.SendOnly:
			return "chan<- (" + typestring(t.Elem()) + ")"
		case types.RecvOnly:
			return "<-chan (" + typestring(t.Elem()) + ")"
		default:
			panic("unknown channel direction")
		}
	case *types.Interface:
		methods := make([]string, t.NumMethods())
		for i := range methods {
			method := t.Method(i)
			methods[i] = method.Name() + signature(method.Type().(*types.Signature))
		}
		return "interface{" + strings.Join(methods, ";") + "}"
	case *types.Map:
		return "map[" + typestring(t.Key()) + "]" + typestring(t.Elem())
	case *types.Named:
		return t.String()
	case *types.Pointer:
		return "*" + typestring(t.Elem())
	case *types.Signature:
		return "func" + signature(t)
	case *types.Slice:
		return "[]" + typestring(t.Elem())
	case *types.Struct:
		fields := make([]string, t.NumFields())
		for i := range fields {
			field := t.Field(i)
			fields[i] = field.Name() + " " + typestring(field.Type())
			if tag := t.Tag(i); tag != "" {
				fields[i] += " " + strconv.Quote(tag)
			}
		}
		return "struct{" + strings.Join(fields, ";") + "}"
	default:
		panic("unknown type: " + t.String())
	}
}
