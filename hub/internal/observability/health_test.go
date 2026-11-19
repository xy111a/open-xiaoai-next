package observability

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fake HTTP 服务：按状态码回包；slow=true 时先睡够 timeout 再回包
// （验证超时报 down、不挂住）。
func fakeHTTPServer(t *testing.T, code int, slow time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow > 0 {
			time.Sleep(slow)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte("{}"))
	}))
}

func TestKWSHealthURL(t *testing.T) {
	cases := map[string]string{
		"ws://127.0.0.1:10400/v1/kws":   "http://127.0.0.1:10400/health",
		"wss://127.0.0.1:10400/v1/kws":  "https://127.0.0.1:10400/health",
		"ws://127.0.0.1:10400":          "http://127.0.0.1:10400/health",
		"ws://127.0.0.1:10400/":         "http://127.0.0.1:10400/health",
		"":                              "",
		"http://127.0.0.1:10400/health": "",
	}
	for in, want := range cases {
		if got := KWSHealthURL(in); got != want {
			t.Fatalf("KWSHealthURL(%q) 应为 %q，实际 %q", in, want, got)
		}
	}
}

func TestDefaultServicesHasSeven(t *testing.T) {
	svcs := DefaultServices(HealthAddrs{
		STT: "127.0.0.1:10300", TTS: "127.0.0.1:10200",
		KWSWSURL: "ws://127.0.0.1:10400/v1/kws", KWSEnabled: true,
		Snapcast: "127.0.0.1:1705", MPD: "127.0.0.1:6600",
		NeteaseAPI: "http://127.0.0.1:3000",
	})
	if len(svcs) != 7 {
		t.Fatalf("应有 7 个服务，实际 %d", len(svcs))
	}
	want := []string{"hub", "whisper", "piper", "kws-sidecar", "snapserver", "mpd", "netease-api"}
	for i, n := range want {
		if svcs[i].Name != n {
			t.Fatalf("第 %d 个服务应为 %s，实际 %s", i, n, svcs[i].Name)
		}
	}
	if svcs[3].Address != "http://127.0.0.1:10400/health" {
		t.Fatalf("kws-sidecar 地址应由 WS 换算成 /health，实际 %q", svcs[3].Address)
	}
	// KWS 未启用：kws 一行仍在，但 Enabled=false（探活时记 disabled）。
	svcs2 := DefaultServices(HealthAddrs{KWSWSURL: "ws://127.0.0.1:10400/v1/kws", KWSEnabled: false})
	if svcs2[3].Enabled {
		t.Fatal("KWS 未启用时 kws-sidecar 的 Enabled 应为 false")
	}
	if len(svcs2) != 7 {
		t.Fatalf("KWS 未启用时也应保留 7 行，实际 %d", len(svcs2))
	}
}

// HTTP 探活：200=ok，500=down，超时=down 且报超时、不挂住。
func TestProbeHTTP(t *testing.T) {
	okSrv := fakeHTTPServer(t, 200, 0)
	defer okSrv.Close()
	errSrv := fakeHTTPServer(t, 500, 0)
	defer errSrv.Close()
	slowSrv := fakeHTTPServer(t, 200, 5*time.Second)
	defer slowSrv.Close()

	c := NewChecker([]Service{
		{Name: "ok", Kind: KindHTTP, Address: okSrv.URL, Enabled: true},
		{Name: "err", Kind: KindHTTP, Address: errSrv.URL, Enabled: true},
		{Name: "slow", Kind: KindHTTP, Address: slowSrv.URL, Enabled: true},
	}, 500*time.Millisecond)
	start := time.Now()
	rep := c.Check(context.Background())
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("一次全量探活应远快于 3s（单服务超时 500ms），实际 %v", elapsed)
	}
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["ok"].Status != StatusOK || byName["ok"].Detail != "HTTP 200" {
		t.Fatalf("200 应为 ok，实际 %+v", byName["ok"])
	}
	if byName["err"].Status != StatusDown || !strings.Contains(byName["err"].Detail, "500") {
		t.Fatalf("500 应为 down 且 detail 含 500，实际 %+v", byName["err"])
	}
	if byName["slow"].Status != StatusDown {
		t.Fatalf("超时应为 down，实际 %+v", byName["slow"])
	}
	if !strings.Contains(strings.ToLower(byName["slow"].Detail), "timeout") &&
		!strings.Contains(byName["slow"].Detail, "deadline") &&
		!strings.Contains(byName["slow"].Detail, "context") {
		t.Fatalf("超时 detail 应说明超时，实际 %q", byName["slow"].Detail)
	}
	if rep.Healthy != 1 || rep.Down != 2 || rep.Total != 3 {
		t.Fatalf("汇总应为 healthy=1 down=2 total=3，实际 %+v", rep)
	}
}

// AcceptAnyStatus：netease-api 根路径回 404 也算通（进程在听）。
func TestProbeHTTPAcceptAnyStatus(t *testing.T) {
	srv := fakeHTTPServer(t, 404, 0)
	defer srv.Close()
	c := NewChecker([]Service{
		{Name: "netease-api", Kind: KindHTTP, Address: srv.URL, Enabled: true, AcceptAnyStatus: true},
		{Name: "strict", Kind: KindHTTP, Address: srv.URL, Enabled: true},
	}, 2*time.Second)
	rep := c.Check(context.Background())
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["netease-api"].Status != StatusOK {
		t.Fatalf("AcceptAnyStatus 时 404 应算 ok，实际 %+v", byName["netease-api"])
	}
	if byName["strict"].Status != StatusDown {
		t.Fatalf("默认只接受 200，404 应为 down，实际 %+v", byName["strict"])
	}
}

// fake Wyoming 服务：读一行 describe，回一行 info；
// mode=wrong 时回错类型；mode=silent 时接了不回（测超时）。
func fakeWyomingServer(t *testing.T, mode string) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				switch mode {
				case "ok":
					_, _ = conn.Write([]byte("{\"type\":\"info\",\"model\":\"small-int8\"}\n"))
				case "wrong":
					_, _ = conn.Write([]byte("{\"type\":\"transcript\",\"text\":\"hi\"}\n"))
				case "silent":
					<-done // 一直不回，等测试结束
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { close(done); ln.Close() }
}

func TestProbeWyoming(t *testing.T) {
	okAddr, closeOK := fakeWyomingServer(t, "ok")
	defer closeOK()
	wrongAddr, closeWrong := fakeWyomingServer(t, "wrong")
	defer closeWrong()
	silentAddr, closeSilent := fakeWyomingServer(t, "silent")
	defer closeSilent()

	c := NewChecker([]Service{
		{Name: "whisper-ok", Kind: KindWyoming, Address: okAddr, Enabled: true},
		{Name: "whisper-wrong", Kind: KindWyoming, Address: wrongAddr, Enabled: true},
		{Name: "whisper-silent", Kind: KindWyoming, Address: silentAddr, Enabled: true},
		{Name: "whisper-dead", Kind: KindWyoming, Address: "127.0.0.1:1", Enabled: true},
	}, 500*time.Millisecond)
	start := time.Now()
	rep := c.Check(context.Background())
	if time.Since(start) > 3*time.Second {
		t.Fatalf("全量探活应远快于 3s，实际 %v", time.Since(start))
	}
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["whisper-ok"].Status != StatusOK || byName["whisper-ok"].Detail != "describe → info" {
		t.Fatalf("describe 回 info 应为 ok，实际 %+v", byName["whisper-ok"])
	}
	if byName["whisper-wrong"].Status != StatusDown {
		t.Fatalf("回包类型不对（transcript）应为 down，实际 %+v", byName["whisper-wrong"])
	}
	if byName["whisper-silent"].Status != StatusDown {
		t.Fatalf("只接不回（超时）应为 down，实际 %+v", byName["whisper-silent"])
	}
	if byName["whisper-dead"].Status != StatusDown {
		t.Fatalf("端口不通应为 down，实际 %+v", byName["whisper-dead"])
	}
}

// fake MPD 服务：先发欢迎行，再对 ping 回 OK；
// badGreeting 时欢迎行不对；ack 时 ping 回 ACK 报错。
func fakeMPDServer(t *testing.T, mode string) (addr string, close func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				switch mode {
				case "badGreeting":
					_, _ = conn.Write([]byte("HELLO\n"))
					return
				default:
					_, _ = conn.Write([]byte("OK MPD 0.23.0\n"))
				}
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				if mode == "ack" {
					_, _ = conn.Write([]byte("ACK [5@0] {} unknown command\n"))
					return
				}
				_, _ = conn.Write([]byte("OK\n"))
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestProbeMPD(t *testing.T) {
	okAddr, closeOK := fakeMPDServer(t, "ok")
	defer closeOK()
	badAddr, closeBad := fakeMPDServer(t, "badGreeting")
	defer closeBad()
	ackAddr, closeAck := fakeMPDServer(t, "ack")
	defer closeAck()

	c := NewChecker([]Service{
		{Name: "mpd-ok", Kind: KindMPD, Address: okAddr, Enabled: true},
		{Name: "mpd-bad", Kind: KindMPD, Address: badAddr, Enabled: true},
		{Name: "mpd-ack", Kind: KindMPD, Address: ackAddr, Enabled: true},
	}, 2*time.Second)
	rep := c.Check(context.Background())
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["mpd-ok"].Status != StatusOK || byName["mpd-ok"].Detail != "ping → OK" {
		t.Fatalf("欢迎行 OK + ping 回 OK 应为 ok，实际 %+v", byName["mpd-ok"])
	}
	if byName["mpd-bad"].Status != StatusDown {
		t.Fatalf("欢迎行不对（非 OK）应为 down，实际 %+v", byName["mpd-bad"])
	}
	if byName["mpd-ack"].Status != StatusDown || !strings.Contains(byName["mpd-ack"].Detail, "ACK") {
		t.Fatalf("ping 回 ACK 应为 down 且 detail 含 ACK，实际 %+v", byName["mpd-ack"])
	}
}

// fake Snapcast 服务：先推一条异步通知，再回 id=1 的正常回包；
// errMode 时回包带 error。
func fakeSnapcastServer(t *testing.T, errMode bool) (addr string, close func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf)
				// 先推一条无 id 的异步通知：探活必须跳过它，不能误认成回包。
				_, _ = conn.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"Client.OnConnect\",\"params\":{}}\n"))
				if errMode {
					_, _ = conn.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32601,\"message\":\"Method not found\"}}\n"))
					return
				}
				_, _ = conn.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"server\":{\"version\":\"1.0\"}}}\n"))
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestProbeSnapcast(t *testing.T) {
	okAddr, closeOK := fakeSnapcastServer(t, false)
	defer closeOK()
	errAddr, closeErr := fakeSnapcastServer(t, true)
	defer closeErr()

	c := NewChecker([]Service{
		{Name: "snap-ok", Kind: KindSnapcast, Address: okAddr, Enabled: true},
		{Name: "snap-err", Kind: KindSnapcast, Address: errAddr, Enabled: true},
	}, 2*time.Second)
	rep := c.Check(context.Background())
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["snap-ok"].Status != StatusOK {
		t.Fatalf("通知后回正常 id=1 回包应为 ok，实际 %+v", byName["snap-ok"])
	}
	if byName["snap-err"].Status != StatusDown || !strings.Contains(byName["snap-err"].Detail, "Method not found") {
		t.Fatalf("回包带 error 应为 down，实际 %+v", byName["snap-err"])
	}
}

// disabled 的服务不探活、不计 down；self 恒 ok；地址为空报 down。
func TestCheckDisabledSelfAndEmpty(t *testing.T) {
	c := NewChecker([]Service{
		{Name: "kws-sidecar", Kind: KindHTTP, Address: "http://127.0.0.1:10400/health", Enabled: false},
		{Name: "hub", Kind: KindSelf, Address: "本进程", Enabled: true},
		{Name: "whisper", Kind: KindWyoming, Address: "", Enabled: true},
	}, 2*time.Second)
	rep := c.Check(context.Background())
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["kws-sidecar"].Status != StatusDisabled {
		t.Fatalf("未启用的服务应为 disabled，实际 %+v", byName["kws-sidecar"])
	}
	if byName["hub"].Status != StatusOK {
		t.Fatalf("Hub 自身应恒 ok，实际 %+v", byName["hub"])
	}
	if byName["whisper"].Status != StatusDown {
		t.Fatalf("地址为空应为 down，实际 %+v", byName["whisper"])
	}
	if rep.Healthy != 1 || rep.Down != 1 || rep.Total != 3 {
		t.Fatalf("汇总应为 healthy=1 down=1 total=3（disabled 不计入），实际 %+v", rep)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("checked_at 不应为空")
	}
	// 顺序与输入一致。
	if rep.Services[0].Name != "kws-sidecar" || rep.Services[2].Name != "whisper" {
		t.Fatalf("结果顺序应与输入一致，实际 %v", rep.Services)
	}
}

// 慢服务不拖住快服务：一个 5s 才回的 HTTP 与一个立即 200 的并跑，
// 全量仍在 ~500ms 超时内结束。
func TestCheckSlowServiceDoesNotBlockOthers(t *testing.T) {
	slowSrv := fakeHTTPServer(t, 200, 5*time.Second)
	defer slowSrv.Close()
	fastSrv := fakeHTTPServer(t, 200, 0)
	defer fastSrv.Close()
	c := NewChecker([]Service{
		{Name: "slow", Kind: KindHTTP, Address: slowSrv.URL, Enabled: true},
		{Name: "fast", Kind: KindHTTP, Address: fastSrv.URL, Enabled: true},
	}, 500*time.Millisecond)
	start := time.Now()
	rep := c.Check(context.Background())
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("慢服务不应拖住全量探活，实际耗时 %v", elapsed)
	}
	byName := map[string]ServiceHealth{}
	for _, h := range rep.Services {
		byName[h.Name] = h
	}
	if byName["fast"].Status != StatusOK {
		t.Fatalf("快服务应为 ok，实际 %+v", byName["fast"])
	}
	if byName["slow"].Status != StatusDown {
		t.Fatalf("慢服务应超时报 down，实际 %+v", byName["slow"])
	}
}
