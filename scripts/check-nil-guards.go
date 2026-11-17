// Command check-nil-guards 是 lint 规则 2 的检查器（go vet 风格，纯标准库）。
//
// 规则 2：STT/TTS/MPD 等接口调用前必须判 nil。同一函数（含闭包）内出现
// `.Transcribe` / `.Synthesize` / `.PlayURL` 调用时，调用的接收者
// （如 p.stt、tts、mpd、r.deps.MPD）必须在同一函数体内出现过
// `== nil` 或 `!= nil` 守卫，否则报错。
// 针对 AUDIT P1-2：Wyoming 拨号失败时 stt/tts 保持 nil 接口，Pipeline 照常
// 创建，首句语音即 nil 接口调用 panic。
//
// 豁免（allowlist，见下）：main.go 里把 wyoming.Client 包一层的两个薄适配器
// wyomingSTT/wyomingTTS——它们只在 Dial 成功后才被构造（c 恒非 nil），nil
// 风险在 voice.STT/voice.TTS 接口层，不在适配器层；接口层的调用点
// （voice.go answer、main.go speakText、router.go music）仍受本规则约束。
//
// 用法：go run scripts/check-nil-guards.go [--hub hub]
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
	"strings"
)

var watchedMethods = map[string]bool{
	"Transcribe": true, "Synthesize": true, "PlayURL": true,
}

// allowlist 的 key 是 "文件名:方法名"。
var allowlist = map[string]bool{
	"main.go:wyomingSTT.Transcribe": true,
	"main.go:wyomingTTS.Synthesize": true,
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
			return nil // 解析失败已告警，跳过
		}
		failures += n
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-nil-guards: %v\n", err)
		os.Exit(2)
	}
	if failures > 0 {
		fmt.Printf("rule2: FAIL（%d 处无 nil 守卫的 STT/TTS/MPD 调用）\n", failures)
		os.Exit(1)
	}
	fmt.Println("rule2: OK（全部 Transcribe/Synthesize/PlayURL 调用都有同一函数内 nil 守卫）")
}

// funcUnit 是一个待检查的函数体（顶层函数或闭包，互不嵌套统计）。
type funcUnit struct {
	name string
	body *ast.BlockStmt
}

// checkFile 检查一个文件，返回 FAIL 数；解析失败返回 -1。
func checkFile(path string) int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN rule2: %s 解析失败（可能正在编辑中），跳过：%v\n", path, err)
		return -1
	}
	var units []funcUnit
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		recv := recvName(fn)
		name := fn.Name.Name
		if recv != "" {
			name = recv + "." + name
		}
		units = append(units, funcUnit{name: name, body: fn.Body})
		// 闭包（如 main.go 的 speakText）单独成单元，不与外层函数共用守卫集。
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok {
				return true
			}
			units = append(units, funcUnit{name: name + " (closure)", body: lit.Body})
			return true // 仍继续找更深的闭包（罕见）
		})
	}
	failures := 0
	for _, u := range units {
		failures += checkUnit(path, u)
	}
	return failures
}

func recvName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
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

// checkUnit 检查一个函数体：收集本体内（不含嵌套闭包）的 nil 守卫，
// 再检查本体内（不含嵌套闭包）的受监视调用。
func checkUnit(path string, u funcUnit) int {
	guards := map[string]bool{}
	collectGuards(u.body, guards)
	failures := 0
	ast.Inspect(u.body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false // 不下钻嵌套闭包（闭包自己是独立单元）
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !watchedMethods[sel.Sel.Name] {
			return true
		}
		receiver := render(sel.X)
		if guards[receiver] {
			return true
		}
		key := filepath.Base(path) + ":" + u.name
		if allowlist[key] {
			return true
		}
		failures++
		fmt.Printf("FAIL rule2 %s: %s() 里 %s.%s() 无 nil 守卫（接收者 %s 在同一函数内无 ==nil/!=nil 检查）\n",
			path, u.name, receiver, sel.Sel.Name, receiver)
		return true
	})
	return failures
}

// collectGuards 收集函数体内（不含嵌套闭包）所有与 nil 比较的表达式。
func collectGuards(body *ast.BlockStmt, guards map[string]bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
			return true
		}
		for _, side := range []ast.Expr{bin.X, bin.Y} {
			if id, ok := side.(*ast.Ident); ok && id.Name == "nil" {
				other := bin.Y
				if side == bin.Y {
					other = bin.X
				}
				guards[render(other)] = true
			}
		}
		return true
	})
}

// render 把表达式渲染成去空格的源码文本，用于“守卫表达式 == 调用接收者”比对。
func render(e ast.Expr) string {
	var buf bytes.Buffer
	fset := token.NewFileSet()
	_ = printer.Fprint(&buf, fset, e)
	return strings.ReplaceAll(buf.String(), " ", "")
}
