// Package resolve 维护 NAS 媒体目录的索引与检索（Library），并在家庭内网
// 提供只读媒体 HTTP 服务（MediaServer）。
//
// 渲染端 APK 无法挂载 NAS：由 cast-agent 就地读取 NAS 文件回传——MediaResolver
// （Task 9）下发播放地址时使用 Library.MediaURL 生成的 http://<lan-ip>:7810/media/<id>
// 形态地址，MediaServer 依索引取出文件用 http.ServeFile 应答。
package resolve

import (
	"crypto/sha1"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 媒体类别（Item.Kind 的取值）。
const (
	KindVideo = "video"
	KindAudio = "audio"
	KindImage = "image"
)

// kindByExt 扩展名 → 类别。Rescan 只收录此表中的扩展名（大小写不敏感）：
// mp4/mkv/mov → video；mp3/flac/wav/m4a → audio；jpg/png → image。
var kindByExt = map[string]string{
	".mp4":  KindVideo,
	".mkv":  KindVideo,
	".mov":  KindVideo,
	".mp3":  KindAudio,
	".flac": KindAudio,
	".wav":  KindAudio,
	".m4a":  KindAudio,
	".jpg":  KindImage,
	".png":  KindImage,
}

// searchMax 单次 Search 返回条数上限。
const searchMax = 20

// Item 是库中的一条媒体。ID/Title 取自扫描时见到的可见文件名
// （符号链接以其链接名入库），ID 为 sha1(可见路径的规范绝对形式) 前 12 位
// 十六进制：路径不变则 ID 跨 Rescan 稳定。Path 是符号链接解析后的真实文件，
// 仅供 MediaServer 服务文件使用，不参与 JSON 序列化（json:"-"），
// 避免向渲染端泄露 NAS 目录结构。
type Item struct {
	ID    string `json:"media_id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	Path  string `json:"-"`
}

// Library 是若干媒体目录的内存索引；读操作（Search/Get）走读锁，
// Rescan 一次性重建并整体替换索引（写锁）。
type Library struct {
	dirs  []string
	mu    sync.RWMutex
	index map[string]Item // id → Item
}

// NewLibrary 创建指向 dirs 的库（内部持有副本，防外部修改）；
// 索引为空，需先 Rescan 才有内容。
func NewLibrary(dirs []string) *Library {
	dirsCopy := make([]string, len(dirs))
	copy(dirsCopy, dirs)
	return &Library{dirs: dirsCopy, index: make(map[string]Item)}
}

// Rescan 递归扫描全部媒体目录并重建索引（成功后写锁内一次性替换）。
//
//   - 仅收录 kindByExt 中的扩展名（大小写不敏感）；
//   - 文件符号链接被跟随：条目以链接名/链接路径入库（Title、ID），而
//     Item.Path 记录 filepath.EvalSymlinks 解析出的真实文件用于服务；
//     失效符号链接（解析失败）跳过，不视为错误；
//   - 目录符号链接不跟随：filepath.WalkDir 默认不进入符号链接目录，
//     因此不可能产生目录环（根目录本身是符号链接时先归一化为真实目录）；
//   - 单个条目不可读（如无权限的子目录/文件）：记 Warn 日志并跳过，其余
//     内容照常收录，不作为 Rescan 的错误返回；
//   - 遍历本身无法开始（如某个根目录不存在/不可解析）时仍返回错误。
func (l *Library) Rescan() error {
	index := make(map[string]Item)
	for _, dir := range l.dirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		root, err := filepath.EvalSymlinks(absDir) // 根目录符号链接归一化，保证遍历起点真实
		if err != nil {
			return err
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// 单个条目不可读（WalkDir 对读不了的目录/文件以 err 回调）：
				// 记 Warn 并跳过，扫描继续，不令整趟 Rescan 失败。
				slog.Warn("rescan: skip unreadable entry", "path", path, "err", err)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			kind, ok := kindByExt[strings.ToLower(filepath.Ext(path))]
			if !ok {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(path) // 跟随文件符号链接
			if err != nil {
				return nil // 失效符号链接：跳过
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() {
				return nil // 指向目录等非常规目标的"文件"（如 movie.mp4 → 目录）：排除，杜绝目录列表
			}
			sum := sha1.Sum([]byte(path)) // path 已位于归一化根下，为绝对路径
			id := hex.EncodeToString(sum[:])[:12]
			base := filepath.Base(path)
			index[id] = Item{
				ID:    id,
				Title: strings.TrimSuffix(base, filepath.Ext(base)),
				Kind:  kind,
				Path:  resolved,
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.index = index
	l.mu.Unlock()
	return nil
}

// Search 按标题做大小写不敏感的子串匹配，结果按 Title 升序返回至多 20 条；
// q 为空时等价于全量媒体（同样截断）。可安全并发调用。
func (l *Library) Search(q string) []Item {
	needle := strings.ToLower(q)
	l.mu.RLock()
	items := make([]Item, 0, len(l.index))
	for _, it := range l.index {
		if strings.Contains(strings.ToLower(it.Title), needle) {
			items = append(items, it)
		}
	}
	l.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool { return items[i].Title < items[j].Title })
	if len(items) > searchMax {
		items = items[:searchMax]
	}
	return items
}

// Get 按精确 ID 取回一条媒体；未知 ID 返回 ok=false。
func (l *Library) Get(id string) (Item, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	it, ok := l.index[id]
	return it, ok
}

// MediaURL 拼接媒体文件的内网访问地址：baseURL 去掉尾部斜杠后接 /media/<id>。
func (l *Library) MediaURL(baseURL, id string) string {
	return strings.TrimRight(baseURL, "/") + "/media/" + id
}
