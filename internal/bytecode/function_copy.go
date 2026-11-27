package bytecode

// CopyWithoutVMCode is a copy of f with every field but VMCode, which it
// leaves nil, never reading f's. f's other fields are not written once f
// is shared, but VMCode is stored, atomically, at the function's first
// run, which may be on another goroutine while the copy is made: a
// background compile lowers a callee it inlines, which may not have run
// yet, while the runtime's goroutine makes its first call. A plain copy
// (*f) reads VMCode then, a data race. A copy is for code that reads the
// function's description, as the JIT's lowering does (its view of a
// function, which never reads VMCode); the VM's form is f's alone.
//
// TestCopyWithoutVMCode fails when a field is added to Function and not
// copied here.
func (f *Function) CopyWithoutVMCode() *Function {
	return &Function{
		Name:              f.Name,
		ParamCount:        f.ParamCount,
		Length:            f.Length,
		LocalCount:        f.LocalCount,
		Constants:         f.Constants,
		Names:             f.Names,
		Locals:            f.Locals,
		Upvalues:          f.Upvalues,
		EvalScopes:        f.EvalScopes,
		Templates:         f.Templates,
		Kind:              f.Kind,
		Strict:            f.Strict,
		Async:             f.Async,
		Generator:         f.Generator,
		HasRest:           f.HasRest,
		HasSimpleParams:   f.HasSimpleParams,
		UsesArguments:     f.UsesArguments,
		MappedArguments:   f.MappedArguments,
		ForwardsArguments: f.ForwardsArguments,
		ParamsAreLexical:  f.ParamsAreLexical,
		ThisProps:         f.ThisProps,
		CapturesByValue:   f.CapturesByValue,
		PropSites:         f.PropSites,
		UsesThis:          f.UsesThis,
		UsesNewTarget:     f.UsesNewTarget,
		CoerceThis:        f.CoerceThis,
		DirectCall:        f.DirectCall,
		ReturnsReceiver:   f.ReturnsReceiver,
		HasTailCall:       f.HasTailCall,
		IsExprBody:        f.IsExprBody,
		IsModule:          f.IsModule,
		TopLevel:          f.TopLevel,
		HasDirectEval:     f.HasDirectEval,
		Source:            f.Source,
		Anonymous:         f.Anonymous,
		AssignsParams:     f.AssignsParams,
		MakesArguments:    f.MakesArguments,
		SuperForwards:     f.SuperForwards,
		Script:            f.Script,
		Start:             f.Start,
		Debug:             f.Debug,
		Text:              f.Text,
		TraceName:         f.TraceName,
		ArrowThisValue:    f.ArrowThisValue,
		FrameCallee:       f.FrameCallee,
		ReadsMaker:        f.ReadsMaker,
		HasYieldStar:      f.HasYieldStar,
		Reg:               f.Reg,
	}
}
