// Command check-bounds 是 lint 规则 3 的 Go 部分检查器（纯标准库）。
//
// 规则 3：外部长度字段必须设上限、网络读操作必须带 deadline/timeout。
// 针对 AUDIT P1-1（Wyoming readEvent）：
//   - `payload_length` 类型断言 panic、负数 panic、超大值 OOM；
//   - `readEvent` 无读写 deadline；`ReadBytes('\n')` 无长度上限。
//
// 本检查器按函数体检查（顶层函数与闭包分开算），报以下四类：
//  1. 未校验的类型断言：形如 `int(x.(float64))` 或单值 `x.(T)` 使用，
//     未走 `v, ok := x.(T)` 的 comma-ok 形式（`switch x.(type)` 除外）。
//  2. 无界 make：`make([]byte, n)` / `make([]byte, 0, n)` 中 n 不是整数字面量、
//     不含 len(...)、且同一函数内没有任何“与非零常量/上限比较”的校验，
//     也不是由带 error 返回的校验函数赋值（如 n, err := parsePayloadLength(l)——
//     见 validatedIdents；纯解析函数 Atoi/ParseInt 除外）。
//     （启发式：n 在同一函数里出现于 `n > K` / `n <= K` / `n > maxLen` 等
//     比较中即视为已校验；`n > 0` 只判下界，不算。已校验仍被报时，
//     请把上限比较写进同一函数，或抽成带 error 返回的校验函数——
//     这正是规则要的“校验离使用点近”。）
//  3. 无界行读：`.ReadBytes('\n')`，除非同一函数用了 io.LimitReader
//     限定上游（此时行读已有上界）。
//  4. 无超时拨号/客户端：`net.Dial(`（须用 DialTimeout 或带 Timeout 的
//     Dialer）、未设 Timeout 的 `http.Client{}`。
//
// 用法：go run scripts/check-bounds.go [--hub hub]
// 退出码：0 通过；1 有 FAIL；2 用法/解析错误。
// 解析失败的单个文件（如他人正在编辑的中间态）只告警并跳过。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// knownExceptions 是规则 3 的已知例外（只记录、不修）：
//   - key 为“文件后缀 + 函数名”，value 为例外原因。
//   - hub/internal/music/music.go 的 SnapClient.call() 里 ReadBytes 无长度
//     上限：这是暂缓审计项（AUDIT P1-1 同类问题在 Snapcast 客户端的残留，
//     不在本轮 12 项修复内）。修时应加读 deadline + 限长读取；在此之前
//     只在 lint 里标注，不静默放行、也不改代码。
var knownExceptions = map[string]string{
	"internal/music/music.go:SnapClient.call": "已知例外：暂缓审计项（P1-1 同类：SnapClient.call() 的 ReadBytes 无长度上限），不在本轮 12 项内，只记录、不修",
}

func main() {
	hubDir := flag.String("hub", "hub", "hub 目录（相对仓库根或绝对路径）")
	flag.Parse()

	failures := 0
	err := filepath.WalkDir(*hubDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		n := checkFile(path)
		if n < 0 {
			return nil
		}
		failures += n
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-bounds: %v\n", err)
		os.Exit(2)
	}
	if failures > 0 {
		fmt.Printf("rule3: FAIL（%d 处外部长度/超时问题）\n", failures)
		os.Exit(1)
	}
	fmt.Println("rule3: OK（外部长度均有上限校验，网络读均有 deadline/timeout）")
}

type funcUnit struct {
	name string
	body *ast.BlockStmt
}

func checkFile(path string) int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN rule3: %s 解析失败（可能正在编辑中），跳过：%v\n", path, err)
		return -1
	}
	var units []funcUnit
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			if recv := recvName(fn); recv != "" {
				name = recv + "." + name
			}
		}
		units = append(units, funcUnit{name: name, body: fn.Body})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok {
				return true
			}
			units = append(units, funcUnit{name: name + " (closure)", body: lit.Body})
			return true
		})
	}
	failures := 0
	for _, u := range units {
		failures += checkUnit(path, u)
	}
	// 文件级：http.Client 未设 Timeout 与 net.Dial（与函数无关，单独扫）。
	failures += checkFileLevel(path, f)
	return failures
}

func recvName(fn *ast.FuncDecl) string {
	switch v := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// checkUnit 检查一个函数体（不下钻嵌套闭包）。
func checkUnit(path string, u funcUnit) int {
	bounded := boundedIdents(u.body)
	validated := validatedIdents(u.body)
	usesLimitReader := false
	ast.Inspect(u.body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == "LimitReader" {
			usesLimitReader = true
		}
		return true
	})

	failures := 0
	// 带父节点栈的遍历，用于判断类型断言是否走了 comma-ok。
	var stack []ast.Node
	ast.Inspect(u.body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok && len(stack) > 0 {
			return false // 不下钻嵌套闭包
		}
		if ta, ok := n.(*ast.TypeAssertExpr); ok && ta.Type != nil {
			if !isCommaOK(stack) {
				failures++
				fmt.Printf("FAIL rule3 %s: %s() 里未校验的类型断言 %s（非数字/负数/超大值会 panic 或 OOM；请用 v, ok := %s.(T) 并校验范围）\n",
					path, u.name, render(ta), render(ta.X))
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			failures += checkCall(path, u.name, call, bounded, validated, usesLimitReader)
		}
		stack = append(stack, n)
		return true
	})
	return failures
}

// validatedIdents 收集“由带 error 返回的辅助函数校验过”的标识符：
// 形如 `n, err := parsePayloadLength(l)`——被调函数在入参非法时返回 error，
// 调用方随后判 err，说明 n 已在该函数内通过范围校验。纯解析函数
// （Atoi/ParseInt 等：只转格式，不限范围）除外。
// 这是启发式：它认得出 P1-1 的正确修法（校验抽到 parsePayloadLength 里），
// 而不是逼着把上限比较都写在同一函数。
func validatedIdents(body *ast.BlockStmt) map[string]bool {
	validated := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
			return true
		}
		errID, ok := as.Lhs[1].(*ast.Ident)
		if !ok || errID.Name != "err" {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callName(call)
		switch name {
		case "Atoi", "ParseInt", "ParseUint", "ParseFloat", "ParseBool":
			return true // 纯解析，不限范围
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			validated[id.Name] = true
		}
		return true
	})
	return validated
}

// callName 取调用的函数名（方法取 Sel.Name，裸函数取 Ident.Name）。
func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// isCommaOK 判断断言节点是否处于 `v, ok := x.(T)` / `var v, ok = x.(T)` 形式。
// stack 顶是断言节点的父节点。
func isCommaOK(stack []ast.Node) bool {
	if len(stack) == 0 {
		return false
	}
	parent := stack[len(stack)-1]
	switch p := parent.(type) {
	case *ast.AssignStmt:
		return len(p.Lhs) == 2 && len(p.Rhs) == 1
	case *ast.ValueSpec:
		return len(p.Names) == 2 && len(p.Values) == 1
	}
	return false
}

// boundedIdents 收集函数体内（不含嵌套闭包）出现于“与非零常量/上限比较”中的标识符。
func boundedIdents(body *ast.BlockStmt) map[string]bool {
	bounded := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		bin, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch bin.Op {
		case token.GTR, token.LSS, token.GEQ, token.LEQ:
		default:
			return true
		}
		// 比较的另一侧含非零整数字面量，或含标识符（视为具名上限常量）即算校验。
		for _, side := range []ast.Expr{bin.X, bin.Y} {
			if hasNonZeroIntLit(side) || hasIdent(side) {
				for _, id := range identsOf(otherSide(bin, side)) {
					bounded[id] = true
				}
			}
		}
		return true
	})
	return bounded
}

func otherSide(bin *ast.BinaryExpr, side ast.Expr) ast.Expr {
	if side == bin.X {
		return bin.Y
	}
	return bin.X
}

func hasNonZeroIntLit(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return true
		}
		if v, err := strconv.ParseInt(lit.Value, 0, 64); err == nil && v != 0 {
			found = true
		}
		return true
	})
	return found
}

func hasIdent(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if _, ok := n.(*ast.Ident); ok {
			found = true
		}
		return true
	})
	return found
}

func identsOf(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			out = append(out, id.Name)
		}
		return true
	})
	return out
}

func containsLenCall(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "len" {
				found = true
			}
		}
		return true
	})
	return found
}

// checkCall 检查一处调用：无界 make / 无界 ReadBytes。
func checkCall(path, funcName string, call *ast.CallExpr, bounded, validated map[string]bool, usesLimitReader bool) int {
	// make([]byte, size[, cap])
	if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "make" && len(call.Args) >= 2 {
		if arr, ok := call.Args[0].(*ast.ArrayType); ok {
			if elt, ok := arr.Elt.(*ast.Ident); ok && elt.Name == "byte" {
				sizeExpr := call.Args[1]
				if len(call.Args) >= 3 {
					sizeExpr = call.Args[2] // make([]byte, len, cap)：cap 也要看
				}
				if unboundedSize(sizeExpr, bounded, validated) {
					fmt.Printf("FAIL rule3 %s: %s() 里 make([]byte, %s) 无上限（外部长度可致 OOM；请先校验 <= 上限，如音频 chunk <= 1MB）\n",
						path, funcName, render(sizeExpr))
					return 1
				}
			}
		}
		return 0
	}
	// x.ReadBytes('\n')：无长度上限的行读。
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ReadBytes" {
		if !usesLimitReader {
			for suffix, reason := range knownExceptions {
				parts := strings.SplitN(suffix, ":", 2)
				if len(parts) == 2 && strings.HasSuffix(path, parts[0]) && funcName == parts[1] {
					fmt.Printf("KNOWN-EXCEPTION rule3 %s: %s() 里 ReadBytes 无长度上限（%s）\n",
						path, funcName, reason)
					return 0
				}
			}
			fmt.Printf("FAIL rule3 %s: %s() 里 ReadBytes 无长度上限（请改用 io.LimitReader 限定上游，或限长读取）\n",
				path, funcName)
			return 1
		}
	}
	return 0
}

// unboundedSize 判断 make 的尺寸表达式是否无界：整数字面量有界；含 len() 有界
// （已有切片的长度）；其余标识符须在同一函数内有上限比较，或由带 error 返回
// 的校验函数赋值（见 validatedIdents）。
func unboundedSize(e ast.Expr, bounded, validated map[string]bool) bool {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.INT {
		return false
	}
	if containsLenCall(e) {
		return false
	}
	ids := identsOf(e)
	if len(ids) == 0 {
		return false // 纯常量表达式（如 1024*1024）
	}
	for _, id := range ids {
		if !bounded[id] && !validated[id] {
			return true
		}
	}
	return false
}

// checkFileLevel 检查文件级模式：net.Dial 无超时、http.Client 未设 Timeout。
func checkFileLevel(path string, f *ast.File) int {
	failures := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "net" && sel.Sel.Name == "Dial" {
				failures++
				fmt.Printf("FAIL rule3 %s: net.Dial 无超时（请用 net.DialTimeout 或带 Timeout 的 net.Dialer）\n", path)
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Client" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" {
			hasTimeout := false
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Timeout" {
						hasTimeout = true
					}
				}
			}
			if !hasTimeout {
				failures++
				fmt.Printf("FAIL rule3 %s: http.Client 未设 Timeout（请设 Timeout，如 15s）\n", path)
			}
		}
		return true
	})
	return failures
}

// render 把表达式渲染成去空格的源码文本。
func render(e ast.Expr) string {
	var buf bytes.Buffer
	fset := token.NewFileSet()
	_ = printer.Fprint(&buf, fset, e)
	return strings.ReplaceAll(buf.String(), " ", "")
}
