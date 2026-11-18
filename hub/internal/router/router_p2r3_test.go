package router

// 第三轮审计 P2 回归测试（Hub 面）：
// H-11 MusicLyric 负 id 拦一道（Web 层已回 400，本处防其他调用方
// 把负数拼进上游 /lyric?id=）。

import (
	"context"
	"strings"
	"testing"
)

func TestMusicLyricNegativeIDRejected(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	r := New(deps, nil)
	for _, id := range []int64{-5, -1} {
		if _, err := r.MusicLyric(context.Background(), id); err == nil {
			t.Fatalf("MusicLyric(%d) 应报错", id)
		} else if !strings.Contains(err.Error(), "id 非法") {
			t.Fatalf("MusicLyric(%d) 应报“id 非法”，实际 %v", id, err)
		}
	}
}
