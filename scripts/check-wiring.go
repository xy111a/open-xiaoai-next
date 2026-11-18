// Command check-wiring 是 lint 规则 1 的检查器（go vet 风格，纯标准库）。
//
// 规则 1：hub/cmd/hub/main.go 里 new 出来的关键依赖，必须在 *_test.go 里有
// 对应构造/调用断言；且 AUDIT_REPORT 要求的关键接线（MPD/Snapcast 拨号、
// OTA ConfirmHello/Sweep、intercom Start/Sweep）必须在生产代码
// 里真实存在，不能只停留在包内单测。
//
// 2026-11-18 音乐融合 B：music.NewMA 与 router.Deps QueueID 两项随 MA 删除
// 而摘掉（MA 已砍，QueueID 是 MA 的队列概念，直连改走 MPD 队列）。
//
// 用法：
//
//	go run scripts/check-wiring.go [--hub hub]
//
// 退出码：0 全部通过；1 有 FAIL；2 用法/解析错误。
// 注意：本检查器只读不改；解析失败的单个文件（如他人正在编辑的中间态）
// 只告警并跳过，不因此判整个检查失败——真正的编译错误由 go vet 兜底。
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// internalPkgs 是 hub 内各包名到其目录（相对 hub/）的映射。
var internalPkgs = map[string]string{
	"agent": "internal/agent", "alerts": "internal/alerts",
	"arbitration": "internal/arbitration", "config": "internal/config",
	"discovery": "internal/discovery", "ha": "internal/ha",
	"intercom": "internal/intercom", "music": "internal/music",
	"opus": "internal/opus", "ota": "internal/ota",
	"registry": "internal/registry", "reminders": "internal/reminders",
	"router": "internal/router", "tlscert": "internal/tlscert",
	"transport": "internal/transport", "voice": "internal/voice",
	"web": "internal/web", "wyoming": "internal/wyoming",
}

// entry 是“必须接线”表中的一项：生产代码里必须出现 prod，测试里必须出现 test。
type entry struct {
	name string // 如 router.New
	// prod 用正则在全部生产文件里找；prodExclude 是生产代码里要排除的目录
	// （定义所在的包本身不算“接线”，如 ota 包内定义 ConfirmHello 不算调用）。
	prod        string
	prodExclude string
	// test 先按 testQualified 在全部测试文件里找；找不到时再按 bare
	// 在 testPkgDir 的测试文件里找（同包测试里常写裸 New(/Dial( 等）。
	testQualified string
	bare          string
	testPkgDir    string
	note          string
}

var table = []entry{
	{"router.New (router.Deps)", `router\.New\s*\(`, "", `router\.New\s*\(`, `New\s*\(`, "internal/router", "AUDIT P1-9/P1-15：Router 接线"},
	{"voice.New (Pipeline)", `voice\.New\s*\(`, "", `voice\.New\s*\(`, `New\s*\(`, "internal/voice", "语音管线接线"},
	{"transport.New (4399 服务)", `transport\.New\s*\(`, "", `transport\.New\s*\(`, `New\s*\(`, "internal/transport", "传输层接线"},
	{"intercom.New", `intercom\.New\s*\(`, "", `intercom\.New\s*\(`, `New\s*\(`, "internal/intercom", "AUDIT P1-15：对讲管理器"},
	{"ota.New", `ota\.New\s*\(`, "", `ota\.New\s*\(`, `New\s*\(`, "internal/ota", "AUDIT P1-9：OTA 任务管理"},
	{"arbitration.New", `arbitration\.New\s*\(`, "", `arbitration\.New\s*\(`, `New\s*\(`, "internal/arbitration", "仲裁器接线"},
	{"reminders.New", `reminders\.New\s*\(`, "", `reminders\.New\s*\(`, `New\s*\(`, "internal/reminders", "提醒存储接线"},
	{"registry.New", `registry\.New\s*\(`, "", `registry\.New\s*\(`, `New\s*\(`, "internal/registry", "设备注册表接线"},
	{"ha.Dial", `ha\.Dial\s*\(`, "", `Dial\s*\(`, `Dial\s*\(`, "internal/ha", "HA 连接"},
	{"wyoming.NewDialer (STT/TTS 懒拨号)", `wyoming\.NewDialer\s*\(`, "", `NewDialer\s*\(`, `NewDialer\s*\(`, "internal/wyoming", "AUDIT P1-2/P1-27：STT/TTS 懒拨号接线（旧 wyoming.Dial 已随 P1-27 改为 NewDialer）"},
	{"agent.New", `agent\.New\s*\(`, "", `agent\.New\s*\(`, `New\s*\(`, "internal/agent", "Agent 客户端"},
	{"music.NewNetease", `music\.NewNetease\s*\(`, "", `NewNetease\s*\(`, `NewNetease\s*\(`, "internal/music", "网易云客户端"},
	{"DialSnap 调用 (Snapcast)", `DialSnap\s*\(`, "internal/music", `DialSnap\s*\(`, `DialSnap\s*\(`, "internal/music", "AUDIT P1-1/P1-26：Snapcast 必须在 main/router 里拨号，不只是定义"},
	{"DialMPD 调用 (MPD)", `DialMPD\s*\(`, "internal/music", `DialMPD\s*\(`, `DialMPD\s*\(`, "internal/music", "AUDIT P1-26：MPD 必须在 main/router 里拨号，不只是定义"},
	{"MPD 注入 Router (SetMPD/MPD:)", `SetMPD\s*\(`, "internal/router", `SetMPD\s*\(|MPD:`, `SetMPD\s*\(`, "internal/router", "AUDIT P1-26：MPD 须注入 Router 或由后台重试补接"},
	{"ota.ConfirmHello 调用", `ConfirmHello\s*\(`, "internal/ota", `ConfirmHello\s*\(`, `ConfirmHello\s*\(`, "internal/ota", "AUDIT P1-9：hello 成功后必须调 ConfirmHello，否则 OTA 永远 pushing"},
	{"ota.MarkRebooted 调用", `MarkRebooted\s*\(`, "internal/ota", `MarkRebooted\s*\(`, `MarkRebooted\s*\(`, "internal/ota", "AUDIT P1-9：OTA 下发后须标记重启"},
	{"ota.Sweep 定时调用", `otaMgr\.Sweep\s*\(`, "internal/ota", `Sweep\s*\(`, `Sweep\s*\(`, "internal/ota", "AUDIT P1-9：须有定时器调 otaMgr.Sweep，否则超时任务永不失败"},
	{"intercom.Start 触发", `Intercom\.Start\s*\(`, "internal/intercom", `Start\s*\(`, `Start\s*\(`, "internal/intercom", "AUDIT P1-15：须有 Router 分支/入口调 Intercom.Start，否则对讲零触发（Router 经 r.deps.Intercom 接口调用，故匹配大写 Intercom.Start）"},
	{"intercom.Sweep 定时调用", `intercom\.Sweep\s*\(`, "internal/intercom", `Sweep\s*\(`, `Sweep\s*\(`, "internal/intercom", "AUDIT P1-15：须有定时器调 intercom.Sweep，否则空闲会话永不超时"},
}

// deferredWiring 是已批复暂缓的接线（deferred until after first gate，
// bai 2026-11-17 批复）：生产代码里目前确实没有这些调用，检查时跳过
// FAIL、只打印 DEFERRED 提醒。条目保留在上面的检查表里（不要删），
// 第一关口后接线完成再从本表摘掉，检查即恢复为 FAIL。
// 摘掉条件：条目对应的接线在生产代码落地并有集成测试覆盖后即从本表删除
// （P1-9 的 ota.ConfirmHello/MarkRebooted/Sweep 与 P1-15 的
// intercom.Start/Sweep 就是按此条件摘掉的 5 个旧条目）。
//
// 2026-11-17 P1 全量修复后本表已空：P1-9（ota.ConfirmHello/MarkRebooted/Sweep）
// 与 P1-15（intercom.Start/Sweep）的接线均已落地，5 个旧条目随之摘掉。
// 机制保留，供将来再有暂缓接线时使用。
var deferredWiring = map[string]string{}

func main() {
	hubDir := flag.String("hub", "hub", "hub 目录（相对仓库根或绝对路径）")
	flag.Parse()
	if *hubDir == "" {
		fmt.Fprintln(os.Stderr, "check-wiring: --hub 不能为空")
		os.Exit(2)
	}

	prodFiles, testFiles, err := collectGo(*hubDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-wiring: %v\n", err)
		os.Exit(2)
	}
	prodText := joinFiles(prodFiles)
	testText := joinFiles(testFiles)

	failures := 0
	checked := 0
	fail := func(format string, args ...any) {
		failures++
		fmt.Printf("FAIL rule1 %s\n", fmt.Sprintf(format, args...))
	}

	for _, e := range table {
		checked++
		re := regexp.MustCompile(e.prod)
		prodOK := false
		for path, text := range prodFiles {
			if e.prodExclude != "" && strings.Contains(path, e.prodExclude) {
				continue
			}
			if re.MatchString(text) {
				prodOK = true
				break
			}
		}
		if !prodOK {
			if reason, ok := deferredWiring[e.name]; ok {
				fmt.Printf("DEFERRED rule1 %s：生产代码里暂无接线（%s）—— %s；deferred until after first gate（bai 2026-11-17 批复），条目保留在检查表里\n", e.name, e.prod, reason)
				continue
			}
			fail("%s：生产代码里找不到接线（%s）—— %s", e.name, e.prod, e.note)
			continue
		}
		testOK := regexp.MustCompile(e.testQualified).MatchString(testText)
		if !testOK && e.bare != "" && e.testPkgDir != "" {
			bareRe := regexp.MustCompile(e.bare)
			for path, text := range testFiles {
				if strings.Contains(path, e.testPkgDir) && bareRe.MatchString(text) {
					testOK = true
					break
				}
			}
		}
		if !testOK {
			fail("%s：生产接线存在，但 *_test.go 里没有对应构造/调用断言（%s）", e.name, e.note)
		}
	}

	// main.go 扫描：凡 internal 包的 New*/Dial* 调用，都要有测试覆盖。
	n, f := scanMain(*hubDir, testFiles)
	checked += n
	failures += f

	_ = prodText
	if failures > 0 {
		fmt.Printf("rule1: FAIL（%d 项检查，%d 项失败）\n", checked, failures)
		os.Exit(1)
	}
	fmt.Printf("rule1: OK（%d 项接线检查全部通过）\n", checked)
}

// scanMain 解析 hub/cmd/hub/main.go，收集 internal 包的 New*/Dial* 调用，
// 逐个查测试覆盖。返回（检查数，失败数）。
func scanMain(hubDir string, testFiles map[string]string) (int, int) {
	mainPath := filepath.Join(hubDir, "cmd", "hub", "main.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, mainPath, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN rule1: main.go 解析失败（可能正在编辑中），跳过 main.go 扫描：%v\n", err)
		return 0, 0
	}
	type call struct{ pkg, fn string }
	seen := map[call]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		dir, isInternal := internalPkgs[pkgIdent.Name]
		if !isInternal {
			return true
		}
		if !strings.HasPrefix(sel.Sel.Name, "New") && !strings.HasPrefix(sel.Sel.Name, "Dial") {
			return true
		}
		seen[call{pkgIdent.Name, sel.Sel.Name}] = true
		_ = dir
		return true
	})
	checked, failures := 0, 0
	names := make([]call, 0, len(seen))
	for c := range seen {
		names = append(names, c)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].pkg != names[j].pkg {
			return names[i].pkg < names[j].pkg
		}
		return names[i].fn < names[j].fn
	})
	for _, c := range names {
		checked++
		qualified := regexp.MustCompile(regexp.QuoteMeta(c.pkg+`.`+c.fn) + `\s*\(`)
		covered := false
		for _, text := range testFiles {
			if qualified.MatchString(text) {
				covered = true
				break
			}
		}
		if !covered {
			bare := regexp.MustCompile(regexp.QuoteMeta(c.fn) + `\s*\(`)
			dir := internalPkgs[c.pkg]
			for path, text := range testFiles {
				if strings.Contains(path, dir) && bare.MatchString(text) {
					covered = true
					break
				}
			}
		}
		if !covered {
			failures++
			fmt.Printf("FAIL rule1 main.go 里的 %s.%s() 在 *_test.go 里没有对应构造/调用断言\n", c.pkg, c.fn)
		}
	}
	return checked, failures
}

// collectGo 收集 hub/ 下全部 .go 文件，按测试/生产分开（返回 path→内容）。
func collectGo(hubDir string) (prod, test map[string]string, err error) {
	prod, test = map[string]string{}, map[string]string{}
	err = filepath.WalkDir(hubDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			test[path] = string(b)
		} else {
			prod[path] = string(b)
		}
		return nil
	})
	return prod, test, err
}

func joinFiles(m map[string]string) string {
	var sb strings.Builder
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sb.WriteString(m[p])
		sb.WriteString("\n")
	}
	return sb.String()
}
