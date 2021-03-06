/*
================================================================
=  Source code from https://github.com/dominikh/go-tools       =
=  Copyright @ Dominik Honnef (https://github.com/dominikh)    =
================================================================
*/

// Package staticcheck contains a linter for Go source code.
package staticcheck

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Tengfei1010/GCBDetector/callgraph"
	"github.com/Tengfei1010/GCBDetector/callgraph/bbcallgraph"
	"github.com/Tengfei1010/GCBDetector/functions"
	"github.com/Tengfei1010/GCBDetector/lint"
	. "github.com/Tengfei1010/GCBDetector/lint/lintdsl"
	"github.com/Tengfei1010/GCBDetector/ssa"
	"github.com/Tengfei1010/GCBDetector/staticcheck/util"
	"golang.org/x/tools/go/loader"
)

type runeSlice []rune

func (rs runeSlice) Len() int               { return len(rs) }
func (rs runeSlice) Less(i int, j int) bool { return rs[i] < rs[j] }
func (rs runeSlice) Swap(i int, j int)      { rs[i], rs[j] = rs[j], rs[i] }

type Checker struct {
	CheckGenerated bool
	funcDescs      *functions.Descriptions
	deprecatedObjs map[types.Object]string
}

func NewChecker() *Checker {
	return &Checker{}
}

func (*Checker) Name() string   { return "staticcheck" }
func (*Checker) Prefix() string { return "SA" }

func (c *Checker) Funcs() map[string]lint.Func {
	return map[string]lint.Func{
		"SA2000": c.CheckWaitgroupAdd,
		"SA2001": c.CheckEmptyCriticalSection,
		"SA2002": c.CheckConcurrentTesting,
		"SA2003": c.CheckDeferLock,
		"SA2004": c.CheckUnlockAfterLock,
		"SA2005": c.CheckDoubleLock,
		"SA2006": c.CheckAnonRace,
		//"SA2007": c.CheckWaitgroupBlocking,
		"SA2008": c.CheckPrimitiveUsage,
	}
}

func (c *Checker) filterGenerated(files []*ast.File) []*ast.File {
	if c.CheckGenerated {
		return files
	}
	var out []*ast.File
	for _, f := range files {
		if !IsGenerated(f) {
			out = append(out, f)
		}
	}
	return out
}

func (c *Checker) findDeprecated(prog *lint.Program) {
	var docs []*ast.CommentGroup
	var names []*ast.Ident

	doDocs := func(pkginfo *loader.PackageInfo, names []*ast.Ident, docs []*ast.CommentGroup) {
		var alt string
		for _, doc := range docs {
			if doc == nil {
				continue
			}
			parts := strings.Split(doc.Text(), "\n\n")
			last := parts[len(parts)-1]
			if !strings.HasPrefix(last, "Deprecated: ") {
				continue
			}
			alt = last[len("Deprecated: "):]
			alt = strings.Replace(alt, "\n", " ", -1)
			break
		}
		if alt == "" {
			return
		}

		for _, name := range names {
			obj := pkginfo.ObjectOf(name)
			c.deprecatedObjs[obj] = alt
		}
	}

	for _, pkginfo := range prog.Prog.AllPackages {
		for _, f := range pkginfo.Files {
			fn := func(node ast.Node) bool {
				if node == nil {
					return true
				}
				var ret bool
				switch node := node.(type) {
				case *ast.GenDecl:
					switch node.Tok {
					case token.TYPE, token.CONST, token.VAR:
						docs = append(docs, node.Doc)
						return true
					default:
						return false
					}
				case *ast.FuncDecl:
					docs = append(docs, node.Doc)
					names = []*ast.Ident{node.Name}
					ret = false
				case *ast.TypeSpec:
					docs = append(docs, node.Doc)
					names = []*ast.Ident{node.Name}
					ret = true
				case *ast.ValueSpec:
					docs = append(docs, node.Doc)
					names = node.Names
					ret = false
				case *ast.File:
					return true
				case *ast.StructType:
					for _, field := range node.Fields.List {
						doDocs(pkginfo, field.Names, []*ast.CommentGroup{field.Doc})
					}
					return false
				case *ast.InterfaceType:
					for _, field := range node.Methods.List {
						doDocs(pkginfo, field.Names, []*ast.CommentGroup{field.Doc})
					}
					return false
				default:
					return false
				}
				if len(names) == 0 || len(docs) == 0 {
					return ret
				}
				doDocs(pkginfo, names, docs)

				docs = docs[:0]
				names = nil
				return ret
			}
			ast.Inspect(f, fn)
		}
	}
}

func (c *Checker) Init(prog *lint.Program) {
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		c.funcDescs = functions.NewDescriptions(prog.SSA)
		for _, fn := range prog.AllFunctions {
			if fn.Blocks != nil {
				applyStdlibKnowledge(fn)
				ssa.OptimizeBlocks(fn)
			}
		}
		wg.Done()
	}()

	go func() {
		c.deprecatedObjs = map[types.Object]string{}
		c.findDeprecated(prog)
		wg.Done()
	}()

	wg.Wait()
}

func (c *Checker) isInLoop(b *ssa.BasicBlock) bool {
	sets := c.funcDescs.Get(b.Parent()).Loops
	for _, set := range sets {
		if set[b] {
			return true
		}
	}
	return false
}

func applyStdlibKnowledge(fn *ssa.Function) {
	if len(fn.Blocks) == 0 {
		return
	}

	// comma-ok receiving from a time.Tick channel will never return
	// ok == false, so any branching on the value of ok can be
	// replaced with an unconditional jump. This will primarily match
	// `for range time.Tick(x)` loops, but it can also match
	// user-written code.
	for _, block := range fn.Blocks {
		if len(block.Instrs) < 3 {
			continue
		}
		if len(block.Succs) != 2 {
			continue
		}
		var instrs []*ssa.Instruction
		for i, ins := range block.Instrs {
			if _, ok := ins.(*ssa.DebugRef); ok {
				continue
			}
			instrs = append(instrs, &block.Instrs[i])
		}

		for i, ins := range instrs {
			unop, ok := (*ins).(*ssa.UnOp)
			if !ok || unop.Op != token.ARROW {
				continue
			}
			call, ok := unop.X.(*ssa.Call)
			if !ok {
				continue
			}
			if !IsCallTo(call.Common(), "time.Tick") {
				continue
			}
			ex, ok := (*instrs[i+1]).(*ssa.Extract)
			if !ok || ex.Tuple != unop || ex.Index != 1 {
				continue
			}

			ifstmt, ok := (*instrs[i+2]).(*ssa.If)
			if !ok || ifstmt.Cond != ex {
				continue
			}

			*instrs[i+2] = ssa.NewJump(block)
			succ := block.Succs[1]
			block.Succs = block.Succs[0:1]
			succ.RemovePred(block)
		}
	}
}

func hasType(j *lint.Job, expr ast.Expr, name string) bool {
	T := TypeOf(j, expr)
	return IsType(T, name)
}

func isTestMain(j *lint.Job, node ast.Node) bool {
	decl, ok := node.(*ast.FuncDecl)
	if !ok {
		return false
	}
	if decl.Name.Name != "TestMain" {
		return false
	}
	if len(decl.Type.Params.List) != 1 {
		return false
	}
	arg := decl.Type.Params.List[0]
	if len(arg.Names) != 1 {
		return false
	}
	return IsOfType(j, arg.Type, "*testing.M")
}

func selectorX(sel *ast.SelectorExpr) ast.Node {
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		return selectorX(x)
	default:
		return x
	}
}

// cgo produces code like fn(&*_Cvar_kSomeCallbacks) which we don't
// want to flag.
var cgoIdent = regexp.MustCompile(`^_C(func|var)_.+$`)

func consts(val ssa.Value, out []*ssa.Const, visitedPhis map[string]bool) ([]*ssa.Const, bool) {
	if visitedPhis == nil {
		visitedPhis = map[string]bool{}
	}
	var ok bool
	switch val := val.(type) {
	case *ssa.Phi:
		if visitedPhis[val.Name()] {
			break
		}
		visitedPhis[val.Name()] = true
		vals := val.Operands(nil)
		for _, phival := range vals {
			out, ok = consts(*phival, out, visitedPhis)
			if !ok {
				return nil, false
			}
		}
	case *ssa.Const:
		out = append(out, val)
	case *ssa.Convert:
		out, ok = consts(val.X, out, visitedPhis)
		if !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	if len(out) < 2 {
		return out, true
	}
	uniq := []*ssa.Const{out[0]}
	for _, val := range out[1:] {
		if val.Value == uniq[len(uniq)-1].Value {
			continue
		}
		uniq = append(uniq, val)
	}
	return uniq, true
}

func objectName(obj types.Object) string {
	if obj == nil {
		return "<nil>"
	}
	var name string
	if obj.Pkg() != nil && obj.Pkg().Scope().Lookup(obj.Name()) == obj {
		var s string
		s = obj.Pkg().Path()
		if s != "" {
			name += s + "."
		}
	}
	name += obj.Name()
	return name
}

func isName(j *lint.Job, expr ast.Expr, name string) bool {
	var obj types.Object
	switch expr := expr.(type) {
	case *ast.Ident:
		obj = ObjectOf(j, expr)
	case *ast.SelectorExpr:
		obj = ObjectOf(j, expr.Sel)
	}
	return objectName(obj) == name
}

func hasSideEffects(node ast.Node) bool {
	dynamic := false
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			dynamic = true
			return false
		case *ast.UnaryExpr:
			if node.Op == token.ARROW {
				dynamic = true
				return false
			}
		}
		return true
	})
	return dynamic
}

func unwrapFunction(val ssa.Value) *ssa.Function {
	switch val := val.(type) {
	case *ssa.Function:
		return val
	case *ssa.MakeClosure:
		return val.Fn.(*ssa.Function)
	default:
		return nil
	}
}

func shortCallName(call *ssa.CallCommon) string {
	if call.IsInvoke() {
		return ""
	}
	switch v := call.Value.(type) {
	case *ssa.Function:
		fn, ok := v.Object().(*types.Func)
		if !ok {
			return ""
		}
		return fn.Name()
	case *ssa.Builtin:
		return v.Name()
	}
	return ""
}

func hasCallTo(block *ssa.BasicBlock, name string) bool {
	for _, ins := range block.Instrs {
		call, ok := ins.(*ssa.Call)
		if !ok {
			continue
		}
		if IsCallTo(call.Common(), name) {
			return true
		}
	}
	return false
}

func loopedRegexp(name string) CallCheck {
	return func(call *Call) {
		if len(extractConsts(call.Args[0].Value.Value)) == 0 {
			return
		}
		if !call.Checker.isInLoop(call.Instr.Block()) {
			return
		}
		call.Invalid(fmt.Sprintf("calling %s in a loop has poor performance, consider using regexp.Compile", name))
	}
}

func buildTagsIdentical(s1, s2 []string) bool {
	if len(s1) != len(s2) {
		return false
	}
	s1s := make([]string, len(s1))
	copy(s1s, s1)
	sort.Strings(s1s)
	s2s := make([]string, len(s2))
	copy(s2s, s2)
	sort.Strings(s2s)
	for i, s := range s1s {
		if s != s2s[i] {
			return false
		}
	}
	return true
}

func isCallToLock(callCommon *ssa.CallCommon) bool {
	if IsCallTo(callCommon, "(*sync.Mutex).Lock") ||
		IsCallTo(callCommon, "(*sync.RWMutex).RLock") ||
		IsCallTo(callCommon, "(*sync.RWMutex).Lock") {
		return true
	}

	// TODO: maybe has FN
	callStr := strings.ToLower(callCommon.String())
	if strings.Contains(callStr, ".lock(") ||
		strings.Contains(callStr, ".rlock(") {

		// Here we ignore the function which has a parameter
		if len(callCommon.Args) > 1 {
			return false
		}

		return true
	}
	return false
}

func isCallToUnlock(callCommon *ssa.CallCommon) bool {
	if IsCallTo(callCommon, "(*sync.Mutex).Unlock") ||
		IsCallTo(callCommon, "(*sync.RWMutex).RUnlock") ||
		IsCallTo(callCommon, "(*sync.RWMutex).UnLock") {
		return true
	}

	// TODO: maybe has FN
	callStr := strings.ToLower(callCommon.String())
	if strings.Contains(callStr, ".unlock") ||
		strings.Contains(callStr, ".runlock") {
		return true
	}

	return false

}

func getLockPrefix(lockCall *ssa.Call) string {
	if len(lockCall.Common().Args) < 1 {
		lockStr := lockCall.Common().String()
		if strings.Contains(lockStr, "invoke") {
			// invoke t65.Lock()return t65
			start := strings.Index(lockStr, " ")
			end := strings.Index(lockStr, ".")
			if start != -1 && end != -1 {
				return lockStr[start:end]
			}
		}
		return lockCall.Common().String()
	}

	value := lockCall.Common().Args[0]
	return value.String()
}

func collectLockInstrs(function *ssa.Function) map[string][]ssa.Instruction {

	result := make(map[string][]ssa.Instruction)

	for _, bb := range function.Blocks {

		for _, instr := range bb.Instrs {
			call, ok := instr.(*ssa.Call)

			if !ok {
				continue
			}

			if isCallToLock(call.Common()) {
				fmt.Println(call.Common())
				lockValue := getLockPrefix(call)
				result[lockValue] = append(result[lockValue], instr)
			}
		}
	}

	return result

}

func isLockToLockInSameBlock(fLock *ssa.Call, sLock *ssa.Call) bool {

	curBlock := fLock.Block()

	fInstrIndex := -1
	sInstrIndex := -1
	unlockIndex := -1

	for index, ins := range curBlock.Instrs {

		call, ok := ins.(*ssa.Call)

		if !ok {
			continue
		}

		if call == fLock {
			fInstrIndex = index
		}

		if isCallToUnlock(call.Common()) && getLockPrefix(call) == getLockPrefix(fLock) {
			unlockIndex = index
			if (fInstrIndex < unlockIndex && sInstrIndex == -1 && fInstrIndex != -1) ||
				(sInstrIndex < unlockIndex && fInstrIndex == -1 && sInstrIndex != -1) {
				return false
			}
		}

		if call == sLock {
			sInstrIndex = index
		}
	}

	if fInstrIndex < sInstrIndex {
		return true
	}

	return false
}

func (c *Checker) CheckWaitgroupAdd(j *lint.Job) {
	fn := func(node ast.Node) bool {
		g, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		fun, ok := g.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}
		if len(fun.Body.List) == 0 {
			return true
		}
		stmt, ok := fun.Body.List[0].(*ast.ExprStmt)
		if !ok {
			return true
		}
		call, ok := stmt.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fn, ok := ObjectOf(j, sel.Sel).(*types.Func)
		if !ok {
			return true
		}
		if fn.FullName() == "(*sync.WaitGroup).Add" {
			j.Errorf(sel, "should call %s before starting the goroutine to avoid a race",
				Render(j, stmt))
		}
		return true
	}
	for _, f := range j.Program.Files {
		ast.Inspect(f, fn)
	}
}

func (c *Checker) CheckEmptyCriticalSection(j *lint.Job) {
	// Initially it might seem like this check would be easier to
	// implement in SSA. After all, we're only checking for two
	// consecutive method calls. In reality, however, there may be any
	// number of other instructions between the lock and unlock, while
	// still constituting an empty critical section. For example,
	// given `m.x().Lock(); m.x().Unlock()`, there will be a call to
	// x(). In the AST-based approach, this has a tiny potential for a
	// false positive (the second call to x might be doing work that
	// is protected by the mutex). In an SSA-based approach, however,
	// it would miss a lot of real bugs.

	mutexParams := func(s ast.Stmt) (x ast.Expr, funcName string, ok bool) {
		expr, ok := s.(*ast.ExprStmt)
		if !ok {
			return nil, "", false
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			return nil, "", false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil, "", false
		}

		fn, ok := ObjectOf(j, sel.Sel).(*types.Func)
		if !ok {
			return nil, "", false
		}
		sig := fn.Type().(*types.Signature)
		if sig.Params().Len() != 0 || sig.Results().Len() != 0 {
			return nil, "", false
		}

		return sel.X, fn.Name(), true
	}

	fn := func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if len(block.List) < 2 {
			return true
		}
		for i := range block.List[:len(block.List)-1] {
			sel1, method1, ok1 := mutexParams(block.List[i])
			sel2, method2, ok2 := mutexParams(block.List[i+1])

			if !ok1 || !ok2 || Render(j, sel1) != Render(j, sel2) {
				continue
			}
			if (method1 == "Lock" && method2 == "Unlock") ||
				(method1 == "RLock" && method2 == "RUnlock") {
				j.Errorf(block.List[i+1], "empty critical section")
			}
		}
		return true
	}
	for _, f := range j.Program.Files {
		ast.Inspect(f, fn)
	}
}

func (c *Checker) CheckConcurrentTesting(j *lint.Job) {
	for _, ssafn := range j.Program.InitialFunctions {
		for _, block := range ssafn.Blocks {
			for _, ins := range block.Instrs {
				gostmt, ok := ins.(*ssa.Go)
				if !ok {
					continue
				}
				var fn *ssa.Function
				switch val := gostmt.Call.Value.(type) {
				case *ssa.Function:
					fn = val
				case *ssa.MakeClosure:
					fn = val.Fn.(*ssa.Function)
				default:
					continue
				}
				if fn.Blocks == nil {
					continue
				}
				for _, block := range fn.Blocks {
					for _, ins := range block.Instrs {
						call, ok := ins.(*ssa.Call)
						if !ok {
							continue
						}
						if call.Call.IsInvoke() {
							continue
						}
						callee := call.Call.StaticCallee()
						if callee == nil {
							continue
						}
						recv := callee.Signature.Recv()
						if recv == nil {
							continue
						}
						if !IsType(recv.Type(), "*testing.common") {
							continue
						}
						fn, ok := call.Call.StaticCallee().Object().(*types.Func)
						if !ok {
							continue
						}
						name := fn.Name()
						switch name {
						case "FailNow", "Fatal", "Fatalf", "SkipNow", "Skip", "Skipf":
						default:
							continue
						}
						j.Errorf(gostmt, "the goroutine calls T.%s, which must be called in the same goroutine as the test", name)
					}
				}
			}
		}
	}
}

func (c *Checker) CheckDeferLock(j *lint.Job) {

	for _, ssafn := range j.Program.InitialFunctions {
		for _, block := range ssafn.Blocks {
			instrs := FilterDebug(block.Instrs)
			if len(instrs) < 2 {
				continue
			}
			for i, ins := range instrs[:len(instrs)-1] {
				call, ok := ins.(*ssa.Call)
				if !ok {
					continue
				}
				if !isCallToLock(call.Common()) {
					continue
				}

				nins, ok := instrs[i+1].(*ssa.Defer)
				if !ok {
					continue
				}
				if !isCallToLock(nins.Common()) {
					continue
				}
				if call.Common().Args[0] != nins.Call.Args[0] {
					continue
				}
				name := shortCallName(call.Common())
				alt := ""
				switch name {
				case "Lock":
					alt = "Unlock"
				case "RLock":
					alt = "RUnlock"
				}
				j.Errorf(nins, "deferring %s right after having locked already; did you mean to defer %s?", name, alt)
			}
		}
	}
}

func (c *Checker) CheckUnlockAfterLock(j *lint.Job) {

	for _, ssafn := range j.Program.InitialFunctions {
		for _, block := range ssafn.Blocks {

			instrs := FilterDebug(block.Instrs)

			if len(instrs) < 2 {
				continue
			}

			for i, ins := range instrs[:len(instrs)-1] {
				call, ok := ins.(*ssa.Call)
				if !ok {
					continue
				}
				if !isCallToLock(call.Common()) {
					continue
				}
				nins, ok := instrs[i+1].(*ssa.Call)
				if !ok {
					continue
				}
				if !isCallToUnlock(nins.Common()) {
					continue
				}
				if call.Common().Args[0] != nins.Call.Args[0] {
					continue
				}
				name := shortCallName(call.Common())
				alt := ""
				switch name {
				case "Lock":
					alt = "Unlock"
				case "RLock":
					alt = "RUnlock"
				}
				j.Errorf(nins, "Unlock %s right after locking; did you mean to defer %s?", name, alt)
			}
		}
	}
}

func isUnlockBeforeLock(sNode *bbcallgraph.BBNode, lockKey string) bool {
	lockIndex := -1
	unLockIndex := -1

	for index, ins := range sNode.BB.Instrs {
		call, ok := ins.(*ssa.Call)
		if !ok {
			continue
		}
		if isCallToUnlock(call.Common()) && getLockPrefix(call) == lockKey {
			unLockIndex = index
		}

		if isCallToLock(call.Common()) && getLockPrefix(call) == lockKey {
			lockIndex = index
		}
	}

	if unLockIndex < lockIndex {
		return true
	}

	return false
}

func findPath(fNode *bbcallgraph.BBNode, sNode *bbcallgraph.BBNode, lockKey string) bool {
	// unlock is in fNode' block, we need not to search
	isNeededSearch := true
	for _, ins := range fNode.BB.Instrs {
		call, ok := ins.(*ssa.Call)
		if !ok {
			continue
		}
		if isCallToUnlock(call.Common()) && getLockPrefix(call) == lockKey {
			isNeededSearch = false
		}
	}

	// unlock is before second lock, we need not to search
	/*
	   func f1 () {
		 r.Lock()
	     ....
	     f2()
	     ....
	   }


	   func f2() {
		  ....
		  r.Unlock()
	      ....
	      r.lock()
		  ....
	   }
	 */

	if isUnlockBeforeLock(sNode, lockKey) {
		isNeededSearch = false
	}

	if isNeededSearch {
		result := bbcallgraph.LockPathSearch(
			fNode, sNode, lockKey, func(node *bbcallgraph.BBNode) bool {

				for _, ins := range node.BB.Instrs {
					call, ok := ins.(*ssa.Call)
					if !ok {
						continue
					}

					if isCallToUnlock(call.Common()) && getLockPrefix(call) == lockKey {
						return false
					}

					if isCallToLock(call.Common()) && getLockPrefix(call) == lockKey {
						break
					}
				}
				return true

			})

		if len(result) > 0 {
			return true
		}
	}

	return false
}

func (c *Checker) _isDoubleLock(fInstr *ssa.Call, sInstr *ssa.Call, lockKey string) bool {

	// TODO: right?
	fName := shortCallName(fInstr.Common())
	sName := shortCallName(sInstr.Common())
	if fName != sName {
		return false
	}

	fFunc := fInstr.Parent()
	sFunc := sInstr.Parent()

	isNotNeedFindPathSearch := false

	// create basic block call graph
	bg := bbcallgraph.BBCallGraph(fFunc)

	if fInstr.Block() == sInstr.Block() {
		if isLockToLockInSameBlock(fInstr, sInstr) {
			isNotNeedFindPathSearch = true
		}

		// maybe in a loop
		if !isNotNeedFindPathSearch && c.isInLoop(fInstr.Block()) {
			fNode := bg.CreateBBNode(fInstr.Block())
			sNode := bg.CreateBBNode(sInstr.Block())
			isNotNeedFindPathSearch = findPath(fNode, sNode, lockKey)
		}

	} else if fFunc == sFunc {
		// in the same function
		/* not is same block, find a path
		func f() {
			a : =0
			r.Lock()
			a = 10
			if i > 0 {
				r2 = f2()
				if r2 > 10 {
					r.Lock()
					a = a - 4
					......
				}
			}
		}
		*/
		fNode := bg.CreateBBNode(fInstr.Block())
		sNode := bg.CreateBBNode(sInstr.Block())
		isNotNeedFindPathSearch = findPath(fNode, sNode, lockKey)
	}

	if !isNotNeedFindPathSearch {

		/*
		func f1() {
		  r.Lock()
		  .....     # no unlock
		  f2()
		  ....
		}

		func f2() {
		  ......    # no unlock
		  r.Lock()
		  ......
		}
		 */

		fFuncNode := c.funcDescs.CallGraph.CreateNode(fFunc)
		//fmt.Println(fFunc.Name() + "---->" + sFunc.Name())

		pathResult := callgraph.PathSearchIgnoreGoCall(
			fFuncNode, func(other *callgraph.Node) bool {
				return other.Func == sFunc
			})

		// Careful pathResult != nil is not equal len(pathResult) > 0
		if len(pathResult) > 0 {

			// TODO: optimize it!!!
			sNode := bg.CreateBBNode(sInstr.Block())
			if isUnlockBeforeLock(sNode, lockKey) {
				// if there is an unlock before second lock, we should ignore it?
				return false
			}

			firstEdge := pathResult[0]
			callInstruction := firstEdge.Site
			sInstr, ok := callInstruction.(*ssa.Call)
			if !ok {
				return false
			}
			// no unlock from lockInstruction to callInstruction
			// no unlock before second locking, see line#977
			if fInstr.Block() == sInstr.Block() {
				if isLockToLockInSameBlock(fInstr, sInstr) {
					return true
				}
			} else {

				fNode := bg.CreateBBNode(fInstr.Block())
				sNode := bg.CreateBBNode(sInstr.Block())

				finded := findPath(fNode, sNode, lockKey)

				//if finded {
				//	fmt.Println(pathResult)
				//}
				return finded
			}
		}
	}
	return isNotNeedFindPathSearch
}

func (c *Checker) CheckDoubleLock(j *lint.Job) {

	lockInstructions := make(map[string][]ssa.Instruction)

	for _, ssafn := range j.Program.InitialFunctions {

		//if !(ssafn.Name() == "checkGrowBaseDeviceFS" || ssafn.Name() == "removeDevice") {
		//	continue
		//}

		lockResultBB := collectLockInstrs(ssafn)

		for lockKey, lockInstrs := range lockResultBB {
			// collect all lock acquiring
			lockInstructions[lockKey] = append(lockInstructions[lockKey], lockInstrs...)
		}
	}

	for lockKey, lockInstrs := range lockInstructions {

		for i := 0; i < len(lockInstrs); i++ {

			for t := i; t < len(lockInstrs); t++ {

				fInstr, _ := lockInstrs[i].(*ssa.Call)
				sInstr, _ := lockInstrs[t].(*ssa.Call)

				if c._isDoubleLock(fInstr, sInstr, lockKey) {

					po1 := j.Program.DisplayPosition(fInstr.Pos())
					po := j.Program.DisplayPosition(sInstr.Pos())
					name := shortCallName(fInstr.Common())
					j.Errorf(fInstr, "Acquiring the %s again at %v, %v", name, po, po1)
				}

				if fInstr != sInstr && c._isDoubleLock(sInstr, fInstr, lockKey) {

					po := j.Program.DisplayPosition(fInstr.Pos())
					name := shortCallName(sInstr.Common())
					j.Errorf(sInstr, "Acquiring the %s again at %v ", name, po)
				}
			}
		}
	}
}

func (c *Checker) CheckAnonRace(j *lint.Job) {

	for _, ssafn := range j.Program.InitialFunctions {

		if strings.HasSuffix(ssafn.String(), ".init") {
			continue
		}

		blockReachability := util.MapReachableBlocks(ssafn)

		if result, ok := util.HasAnonRace(ssafn.AnonFuncs, blockReachability); ok {
			fmt.Println(result)
		}
	}

}

func (c *Checker) CheckWaitgroupBlocking(j *lint.Job) {

	for _, ssafn := range j.Program.InitialFunctions {

		// for loop in a func and create goroutines in the loop
		loopSets := c.funcDescs.Get(ssafn).Loops

		for _, loop := range loopSets {

			isCallDoneInGoroutine := false
			isCallWait := false

			for bb := range loop {

				for _, ins := range bb.Instrs {

					// new goroutine
					gostmt, ok := ins.(*ssa.Go)

					if ok {

						var fn *ssa.Function
						switch val := gostmt.Call.Value.(type) {
						case *ssa.Function:
							fn = val
						case *ssa.MakeClosure:
							fn = val.Fn.(*ssa.Function)
						default:
							continue
						}
						if fn.Blocks == nil {
							continue
						}

						for _, block := range fn.Blocks {
							for _, ins := range block.Instrs {
								call, ok := ins.(*ssa.Call)
								if !ok {
									continue
								}

								callStr := strings.ToLower(call.Common().String())
								if strings.Contains(callStr, ".done(") {
									isCallDoneInGoroutine = true
								}
							}
						}
					}

					// call Wait()
					call, ok := ins.(*ssa.Call)
					if ok {
						callStr := strings.ToLower(call.Common().String())
						if strings.Contains(callStr, ".wait(") {
							isCallWait = true
						}
					}
				}
			}

			if isCallWait && isCallDoneInGoroutine {

				for bb, ok := range loop {

					if ok {

						for _, ins := range bb.Instrs {
							if ins.Pos() > 0 {
								j.Errorf(ins, "There is a potential blocking bug,"+
									"which caused by misusing Wait() and Done()!")
								break
							}
						}
					}
				}
			}
		}
	}
}

func _CallName(call *ssa.CallCommon) string {

	if call.IsInvoke() {
		return call.String()
	}

	switch v := call.Value.(type) {
	case *ssa.Function:
		fn, ok := v.Object().(*types.Func)
		if !ok {
			return ""
		}
		return fn.FullName()
	case *ssa.Builtin:
		return v.Name()
	}
	return ""
}

func ignoreFunc(j *lint.Job, f *ssa.Function) bool {

	for _, bb := range f.Blocks {

		for _, ins := range bb.Instrs {
			if ins.Pos() > 0 {
				po := j.Program.DisplayPosition(ins.Pos()).String()

				if strings.Contains(po, "_test.go") {
					return true
				} else {
					return false
				}
			}
		}
	}
	return false
}

func (c *Checker) CheckPrimitiveUsage(j *lint.Job) {
	isMutex := 0
	isRWMutex := 0
	isCond := 0
	isPool := 0
	isWaitgroup := 0
	isAtomic := 0
	isOnce := 0
	isChannel := 0

	for _, ssafn := range j.Program.InitialFunctions {

		// ignore test file
		if ignoreFunc(j, ssafn) {
			continue
		}

		for _, bb := range ssafn.Blocks {

			instrs := FilterDebug(bb.Instrs)

			for _, ins := range instrs {

				// Send type
				// send value to channel
				_, ok := ins.(*ssa.Send)
				if ok {
					isChannel += 1
					//fmt.Println(ins)
					continue
				}

				// UnOp type
				unop, ok := ins.(*ssa.UnOp)
				if ok {
					if unop.Op == token.ARROW {
						isChannel += 1
						//fmt.Println(ins)
						continue
					}
				}

				// channel in select
				selector, ok := ins.(*ssa.Select)
				if ok {
					// if each case in select is related to a channel
					for _, state := range selector.States {
						if state.Chan != nil {
							isChannel += 1
						}
					}
					continue
				}

				// call
				var call *ssa.CallCommon

				call_, ok := ins.(*ssa.Call)

				if ok {
					call = call_.Common()
				}

				deferIns, ok := ins.(*ssa.Defer)
				if ok {
					call = deferIns.Common()
				}

				if call != nil {
					callName := _CallName(call)
					if callName == "(*sync.Mutex).Lock" || callName == "(*sync.Mutex).Unlock" {
						isMutex += 1
						continue
					}

					if callName == "(*sync.RWMutex).Lock" || callName == "(*sync.RWMutex).Unlock" ||
						callName == "(*sync.RWMutex).RLock" || callName == "(*sync.RWMutex).RUnlock" {
						isRWMutex += 1
						continue
					}

					if callName == "(*sync.WaitGroup).Add" || callName == "(*sync.WaitGroup).Done" ||
						callName == "(*sync.WaitGroup).Wait" {
						isWaitgroup += 1
						continue
					}

					if callName == "(*sync.Once).Do" {
						isOnce += 1
						continue
					}

					if callName == "(*sync.Cond).Broadcast" || callName == "(*sync.Cond).Signal" ||
						callName == "(*sync.Cond).Wait" {
						isCond += 1
						continue
					}

					if callName == "(*sync.Pool).Get" || callName == "(*sync.Pool).Put" {
						isPool += 1
						continue
					}

					if strings.Contains(callName, "atomic") {
						isAtomic += 1
						continue
					}
				}
			}
		}
	}

	fmt.Printf("Mutex: %d, RWMutex %d,Cond %d, Pool %d, Once %d, atomic %d, Waitgroup %d, Channel %d\n",
		isMutex, isRWMutex, isCond, isPool, isOnce, isAtomic, isWaitgroup, isChannel)
}
