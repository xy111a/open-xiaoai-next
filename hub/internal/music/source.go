// Package music 的音源插件层：点歌的“去哪找歌”抽象成 Source 接口，
// Router 只认接口，不认具体音源。内置两个音源：
//   - netease：网易云直连（主路，搜歌 + 并行取可播地址）；
//   - local：MPD 本地曲库（/volume1/music，兜底）。
//
// Chain 按配置顺序逐个尝试：主路找不到/出错即落到下一个；
// 加新音源只需实现 Source 接口并在 Chain 里排进去，不改 Router。
package music

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"
)

// ErrNoPlayable 是 NeteaseSource 在“搜到了歌、但一首都没有可播地址”
// （VIP/无版权）时返回的哨兵错误：Chain 收到它会继续试下一个音源，
// Router 收到它能明确回复“网易云没有可播地址”。
var ErrNoPlayable = errors.New("music: 搜到了歌但都没有可播地址")

// Playable 是一首能播的歌：URL 与 File 二选一。
// URL 是 http(s) 可播地址（网易云），走 MPD.PlayURL；
// File 是 MPD 本地曲库里的相对路径，走 MPD.PlayFile。
type Playable struct {
	Song NeteaseSong // 歌名/歌手（本地歌的 ID 为 0，无网易云 ID）
	URL  string      // 网易云可播地址；本地歌为空
	File string      // MPD 本地文件路径；网易云歌为空
}

// Source 是一个音源插件：按关键词找能播的歌。
// Search 返回空 slice + nil error 表示“这个音源里没有”
// （Chain 会继续试下一个音源）；返回 error 表示“这个音源出故障了”
// （Chain 同样继续试下一个，并记日志）。
type Source interface {
	// Name 是音源名（"netease" / "local"），也是 Chain 里
	// SearchSource 按名取音源的键与 currentTrack 的来源标记。
	Name() string
	// Search 找最多 limit 首能播的歌（已按优先级排好序）。
	Search(ctx context.Context, query string, limit int) ([]Playable, error)
}

// NeteaseSource 是网易云音源：NeteaseClient 的 Source 适配器。
type NeteaseSource struct {
	client *NeteaseClient
}

// NewNeteaseSource 把已有的 NeteaseClient 包成 Source。
func NewNeteaseSource(c *NeteaseClient) *NeteaseSource {
	return &NeteaseSource{client: c}
}

// Name 返回 "netease"。
func (s *NeteaseSource) Name() string { return "netease" }

// Search 搜歌并并行取可播地址：没地址的歌（VIP/无版权）直接跳过，
// 只返回真能播的；搜不到或都没地址时返回空 slice（不报错），
// 让 Chain 继续试本地兜底。
func (s *NeteaseSource) Search(ctx context.Context, query string, limit int) ([]Playable, error) {
	songs, err := s.client.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if len(songs) == 0 {
		return nil, nil
	}
	urls := fetchSongURLs(ctx, s.client, songs)
	var out []Playable
	for i, song := range songs {
		if urls[i] == "" {
			continue
		}
		out = append(out, Playable{Song: song, URL: urls[i]})
	}
	if len(out) == 0 {
		// 搜到了歌但一首都没地址（VIP/无版权）：不是“没搜到”，
		// 返回哨兵错误让 Chain 继续试本地、Router 明说原因。
		return nil, ErrNoPlayable
	}
	return out, nil
}

// fetchSongURLs 并行取多首歌的可播地址（原 router.fetchSongURLs，
// 随 NeteaseSource 一起搬过来）：返回与 songs 等长、按原顺序排列的
// URL（取不到为空字符串），保证“搜到的第一首优先”。
func fetchSongURLs(ctx context.Context, netease *NeteaseClient, songs []NeteaseSong) []string {
	urls := make([]string, len(songs))
	var wg sync.WaitGroup
	for i, song := range songs {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			u, err := netease.SongURL(ctx, id)
			if err != nil {
				log.Printf("netease: 取地址失败（歌曲 %d）: %v", id, err)
				return
			}
			urls[i] = u
		}(i, song.ID)
	}
	wg.Wait()
	return urls
}

// LocalSource 是 MPD 本地曲库音源：按文件名在 MPD 里找歌，
// 找到即用 MPD.PlayFile 播（不走任何 URL）。
type LocalSource struct {
	mpd    *MPDClient
	folder string // music_folder，仅用于 Available 判断，不拼路径
}

// NewLocalSource 新建本地音源（mpd 可为 nil：此时 Search 直接报错，
// Chain 会跳过它；Router 的 SetMPD 补接后需重建 Chain——
// Router 是每次点歌现建 Chain，所以热换自动生效）。
func NewLocalSource(mpd *MPDClient, folder string) *LocalSource {
	return &LocalSource{mpd: mpd, folder: folder}
}

// Name 返回 "local"。
func (s *LocalSource) Name() string { return "local" }

// Available 判断本地曲库是否可用（MPD 已接 + 文件夹是目录）。
// 不可用时 Search 直接报错，不发 MPD 命令。
func (s *LocalSource) Available() bool {
	if s.mpd == nil || s.folder == "" {
		return false
	}
	return localFolderIsDir(s.folder)
}

// Search 按文件名找歌：先试歌名部分（“周杰伦的青花瓷”→“青花瓷”），
// 再试整句；返回最多 limit 首（按 MPD 返回顺序）。
func (s *LocalSource) Search(ctx context.Context, query string, limit int) ([]Playable, error) {
	if !s.Available() {
		return nil, fmt.Errorf("local: 本地曲库不可用（MPD 未接或 music_folder 不是目录）")
	}
	if limit <= 0 {
		limit = 1
	}
	_, track := splitArtistTrack(query)
	seen := map[string]bool{}
	var out []Playable
	for _, term := range []string{track, query} {
		if strings.TrimSpace(term) == "" || len(out) >= limit {
			continue
		}
		// P1-11：总预算到了就停，不再开第二轮 SearchFile（每轮最多 5s）。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files, err := s.mpd.SearchFile(term)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if seen[f] || len(out) >= limit {
				continue
			}
			seen[f] = true
			out = append(out, Playable{Song: songFromFile(f, query), File: f})
		}
	}
	return out, nil
}

// songFromFile 由本地文件名拼展示用的歌名：去掉目录与扩展名；
// 歌手沿用 query 里“的”前面的部分（有的话）。
func songFromFile(file, query string) NeteaseSong {
	artist, track := splitArtistTrack(query)
	base := path.Base(file)
	name := strings.TrimSuffix(base, path.Ext(base))
	if track != "" && strings.Contains(file, track) {
		name = track
	}
	return NeteaseSong{Name: name, Artist: artist}
}

// localFolderIsDir 判断 music_folder 是否为可用目录
// （与 config.MusicFolderWarning 同一判定，单独抽出来供本地音源用）。
func localFolderIsDir(folder string) bool {
	fi, err := os.Stat(folder)
	return err == nil && fi.IsDir()
}

// splitArtistTrack 把“周杰伦的青花瓷”拆成歌手 + 歌名；
// 没“的”或任一侧为空时整句当歌名、歌手为空。
// （原 router.splitArtistTrack，本地音源搜歌也要用，搬到 music 包。）
func splitArtistTrack(query string) (artist, track string) {
	i := strings.Index(query, "的")
	if i <= 0 {
		return "", query
	}
	artist = strings.TrimSpace(query[:i])
	track = strings.TrimSpace(query[i+len("的"):])
	if artist == "" || track == "" {
		return "", query
	}
	return artist, track
}

// Chain 是按顺序尝试的音源链：主路找不到/出错即落到下一个。
// 空 Chain 的 Search 返回空 slice + nil error（调用方按“没配音源”处理）。
type Chain struct {
	sources []Source
	// mu 保护 failed：Chain 可能被多路语音并发复用（测试注入的
	// MusicSources 是共享实例），failed 的读写须串行化。
	mu sync.Mutex
	// failed 是最近一次 Search 里出错的音源名（P1-11，2026-11-23 审查）：
	// 调用方据此判断某音源是否已经试过、失败了——失败过的音源不要再
	// 单独重试（本地音源 SearchFile 一轮最多 5s，重复调就是重复拖时间）。
	failed map[string]bool
}

// Failed 报告 name 音源在最近一次 Search 里是否出错过。
func (c *Chain) Failed(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed[name]
}

// NewChain 按给定顺序建链（调用方按 config music.primary/fallback 排）。
func NewChain(sources ...Source) *Chain {
	var live []Source
	for _, s := range sources {
		if s != nil {
			live = append(live, s)
		}
	}
	return &Chain{sources: live}
}

// Len 返回链里的音源个数（0 表示没配任何音源）。
func (c *Chain) Len() int { return len(c.sources) }

// Search 按顺序试每个音源，返回第一个有结果的音源的可播歌
// 与它的名字；都找不到返回空 slice + nil error；都出错返回最后一个错误。
// 单个音源出错只记日志、不中断整条链；出错的音源名记进 failed，
// 供调用方判断某音源是否已试过（P1-11）。ctx 已过期时不再开新音源
// 的搜索——本地音源的 SearchFile 不接 ctx，过期了也会跑满两轮。
func (c *Chain) Search(ctx context.Context, query string, limit int) ([]Playable, string, error) {
	c.mu.Lock()
	c.failed = map[string]bool{}
	c.mu.Unlock()
	var lastErr error
	for _, s := range c.sources {
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		got, err := s.Search(ctx, query, limit)
		if err != nil {
			log.Printf("music: 音源 %s 搜索 %q 失败，试下一个: %v", s.Name(), query, err)
			c.mu.Lock()
			c.failed[s.Name()] = true
			c.mu.Unlock()
			lastErr = err
			continue
		}
		if len(got) == 0 {
			continue
		}
		return got, s.Name(), nil
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", nil
}

// SearchSource 只问链里指定名字的音源（点歌超时后的“先试本地”
// 兜底用它，不再经过主路）；找不到该音源返回错误。
func (c *Chain) SearchSource(ctx context.Context, name, query string, limit int) ([]Playable, error) {
	for _, s := range c.sources {
		if s.Name() == name {
			return s.Search(ctx, query, limit)
		}
	}
	return nil, fmt.Errorf("music: 链里没有 %q 音源", name)
}
