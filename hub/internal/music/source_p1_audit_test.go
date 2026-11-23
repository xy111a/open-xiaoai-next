package music

import (
	"context"
	"errors"
	"testing"
)

// P1-11（2026-11-23 审查）：Chain 记失败音源——调用方据此判断某音源
// 是否已经试过、失败了，避免点歌兜底里重复调同一个已失败的音源。
func TestChainFailedTracksErrors(t *testing.T) {
	boom := errors.New("boom")
	// 两音源都失败：都记进 Failed。
	chain := NewChain(
		&stubSource{name: "netease", err: boom},
		&stubSource{name: "local", err: boom},
	)
	if _, _, err := chain.Search(context.Background(), "青花瓷", 3); err == nil {
		t.Fatal("两音源都失败时 Search 应返回错误")
	}
	if !chain.Failed("netease") || !chain.Failed("local") {
		t.Fatal("出错的音源都应记进 Failed")
	}
	// 只 netease 失败、本地搜到歌：本地没失败，不应记。
	chain2 := NewChain(
		&stubSource{name: "netease", err: boom},
		&stubSource{name: "local", playables: []Playable{{Song: NeteaseSong{Name: "青花瓷"}}}},
	)
	if _, name, err := chain2.Search(context.Background(), "青花瓷", 3); err != nil || name != "local" {
		t.Fatalf("本地搜到歌时应返回本地结果，实际 name=%q err=%v", name, err)
	}
	if !chain2.Failed("netease") {
		t.Fatal("netease 出错应记进 Failed")
	}
	if chain2.Failed("local") {
		t.Fatal("本地没出错，不应记进 Failed")
	}
	// 本地搜不到（空结果、无错）：空结果不是失败，不记 Failed——
	// 调用方仍可单独重试本地。
	chain3 := NewChain(&stubSource{name: "local"})
	if _, _, err := chain3.Search(context.Background(), "青花瓷", 3); err != nil {
		t.Fatalf("都空结果时 Search 应返回 nil error，实际 %v", err)
	}
	if chain3.Failed("local") {
		t.Fatal("本地空结果不是失败，不应记进 Failed")
	}
}
