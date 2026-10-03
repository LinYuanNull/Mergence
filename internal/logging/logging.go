// logging.go ModelMux 结构化日志基座。
//
// 解决四个具体问题：
//  1. 无结构 —— 全部走 log/slog，字段化输出，文件为 JSONL（可 jq/grep）
//  2. 面板只能看字符串 —— 内存环形缓冲里存的是**结构化 Entry 对象**，不是格式化后的行
//  3. 多进程日志必串 —— 子进程 stdout/stderr 经 ProviderWriter 归一，每行强制带 provider 标签
//  4. 无请求链路 —— req_id 经 context 透传，Handler 自动从 ctx 提取注入
//
// 不引入任何第三方依赖。
package logging

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------- req_id

type ctxKey int

const reqIDKey ctxKey = iota

// NewReqID 生成一个短小、可读、足以在单机内避免碰撞的请求 ID。
func NewReqID() string {
	return fmt.Sprintf("r%x-%04x", time.Now().UnixMilli()&0xFFFFFFF, seq.Add(1)&0xFFFF)
}

var seq atomic.Uint32

// WithReqID 把 req_id 放进 context。
func WithReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey, id)
}

// ReqIDFrom 从 context 取出 req_id，没有则返回空串。
func ReqIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(reqIDKey).(string); ok {
		return v
	}
	return ""
}

// ---------------------------------------------------------------- Entry / Ring

// Entry 面板直接消费的结构化日志条目。
type Entry struct {
	Seq      uint64         `json:"seq"`
	Time     time.Time      `json:"time"`
	Level    string         `json:"level"`
	Msg      string         `json:"msg"`
	Provider string         `json:"provider,omitempty"`
	ReqID    string         `json:"req_id,omitempty"`
	Raw      string         `json:"raw,omitempty"` // 子进程原生日志行（非 JSON）
	Fields   map[string]any `json:"fields,omitempty"`
}

// Ring 定长环形缓冲，保留最近 N 条结构化日志。
//
// 存对象而非字符串：面板的筛选、按 req_id 聚合链路都必须在结构化数据上做，
// 事后再去解析文本行既慢又脆。
type Ring struct {
	mu    sync.RWMutex
	buf   []Entry
	head  int // 下一个写入位置
	full  bool
	total uint64
}

func NewRing(size int) *Ring {
	if size <= 0 {
		size = 5000
	}
	return &Ring{buf: make([]Entry, size)}
}

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = r.total
	r.total++
	r.buf[r.head] = e
	r.head = (r.head + 1) % len(r.buf)
	if r.head == 0 {
		r.full = true
	}
}

// Snapshot 返回 since 之后的条目（since=0 表示全量），最多 limit 条。
// 返回的切片按时间正序，末尾是最新的。
func (r *Ring) Snapshot(since uint64, limit int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.head
	if r.full {
		n = len(r.buf)
	}
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		idx := i
		if r.full {
			idx = (r.head + i) % len(r.buf)
		}
		e := r.buf[idx]
		if e.Seq >= since {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// LastSeq 返回已写入的条目总数（下一次 Snapshot 的 since 传它即可增量拉取）。
func (r *Ring) LastSeq() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total
}

// ---------------------------------------------------------------- rotating file

type rotatingFile struct {
	mu      sync.Mutex
	path    string
	maxByte int64
	f       *os.File
	size    int64
}

func newRotatingFile(path string, maxMB int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	rf := &rotatingFile{path: path, maxByte: int64(maxMB) * 1024 * 1024}
	if err := rf.open(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size+int64(len(p)) > r.maxByte {
		_ = r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// ---------------------------------------------------------------- Handler

// handler 一个 Handler 同时喂两个 sink：轮转文件（JSONL）与内存环形缓冲。
type handler struct {
	level slog.Level
	ring  *Ring
	file  *rotatingFile // 可为 nil（纯内存模式）
	attrs []slog.Attr
	group string
	mu    *sync.Mutex // 串行化文件写入
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &nh
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	if nh.group == "" {
		nh.group = name
	} else {
		nh.group += "." + name
	}
	return &nh
}

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	fields := map[string]any{}
	for _, a := range h.attrs {
		putField(fields, h.group, a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		putField(fields, h.group, a)
		return true
	})

	// 关键字段从 fields 提到顶层，方便筛选与前端直接展示。
	provider, _ := fields["provider"].(string)
	reqID := ReqIDFrom(ctx)
	if v, ok := fields["req_id"].(string); ok && v != "" {
		reqID = v
	}
	raw, _ := fields["raw"].(string)

	e := Entry{
		Time:     rec.Time,
		Level:    levelName(rec.Level),
		Msg:      rec.Message,
		Provider: provider,
		ReqID:    reqID,
		Raw:      raw,
		Fields:   fields,
	}
	h.ring.add(e)

	if h.file == nil {
		return nil
	}
	line := map[string]any{
		"ts":    rec.Time.Format(time.RFC3339Nano),
		"level": e.Level,
		"msg":   rec.Message,
	}
	if provider != "" {
		line["provider"] = provider
	}
	if reqID != "" {
		line["req_id"] = reqID
	}
	for k, v := range fields {
		if k == "provider" || k == "req_id" {
			continue
		}
		line[k] = v
	}
	b, err := json.Marshal(line)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.file.Write(append(b, '\n'))
	return err
}

func putField(dst map[string]any, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if group != "" {
		key = group + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			putField(dst, key, g)
		}
		return
	}
	dst[key] = a.Value.Any()
}

func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// ---------------------------------------------------------------- Logger

// Logger 对外门面。
type Logger struct {
	base *slog.Logger
	ring *Ring
	file *rotatingFile
}

// New 按配置创建 Logger。file 写入失败时降级为纯内存模式（不阻断启动）。
func New(level, dir string, ringSize, maxFileMB int) (*Logger, error) {
	ring := NewRing(ringSize)
	h := &handler{
		level: parseLevel(level),
		ring:  ring,
		mu:    &sync.Mutex{},
	}
	if dir != "" {
		rf, err := newRotatingFile(filepath.Join(dir, "modelmux.log"), maxFileMB)
		if err == nil {
			h.file = rf
		}
	}
	return &Logger{
		base: slog.New(h),
		ring: ring,
		file: h.file,
	}, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Slog 暴露底层 slog.Logger，供业务代码打结构化日志。
func (l *Logger) Slog() *slog.Logger { return l.base }

// Ring 返回环形缓冲，供面板 / API 消费。
func (l *Logger) Ring() *Ring { return l.ring }

// Close 关闭文件句柄。
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Convenience helpers —— 让调用点更短，避免到处 slog.String/Int。
func (l *Logger) Debug(msg string, args ...any) { l.base.Debug(msg, args...) }
func (l *Logger) Info(msg string, args ...any)  { l.base.Info(msg, args...) }
func (l *Logger) Warn(msg string, args ...any)  { l.base.Warn(msg, args...) }
func (l *Logger) Error(msg string, args ...any) { l.base.Error(msg, args...) }

// With 返回带固定字段的子 Logger。
func (l *Logger) With(args ...any) *Logger {
	return &Logger{base: l.base.With(args...), ring: l.ring, file: l.file}
}

// WithProvider 返回绑定 provider 标签的子 Logger —— 托管型 provider 的日志一律走它，
// 这样面板里永远能看出「这行是谁说的」。
func (l *Logger) WithProvider(name string) *Logger {
	return l.With("provider", name)
}

// ---------------------------------------------------------------- 子进程日志归一

// ProviderWriter 返回一个 io.Writer，接子进程的 stdout/stderr。
//
// 逐行处理：
//   - 若该行本身是 JSON 且带 level 字段 → 当作结构化日志转发（保留其字段）
//   - 否则 → 包成 {"level":"INFO","raw":"<原行>"}
//
// 两条路径都会打上 provider 标签，杜绝「不知道哪来的裸行」。
func (l *Logger) ProviderWriter(provider string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r\n")
			if strings.TrimSpace(line) == "" {
				continue
			}
			l.forward(provider, line)
		}
	}()
	return pw
}

func (l *Logger) forward(provider, line string) {
	var m map[string]any
	if json.Unmarshal([]byte(line), &m) == nil {
		if _, ok := m["level"]; ok {
			lvl := parseLevel(fmt.Sprint(m["level"]))
			msg, _ := m["msg"].(string)
			args := []any{"provider", provider, "source", "subprocess"}
			for k, v := range m {
				switch k {
				case "level", "msg", "ts", "time":
					continue
				}
				args = append(args, k, v)
			}
			l.base.Log(context.Background(), lvl, msg, args...)
			return
		}
	}
	// 原生非结构化行
	if guess := guessLevel(line); guess >= slog.LevelWarn {
		l.base.Log(context.Background(), guess, "子进程原始输出",
			"provider", provider, "source", "subprocess", "raw", line)
		return
	}
	l.base.Info("子进程原始输出",
		"provider", provider, "source", "subprocess", "raw", line)
}

// guessLevel 从原生日志行里粗判级别 —— 很多上游把 ERROR 塞在纯文本里，
// 不做这一次猜测的话面板的「只看错误」会漏掉它们。
func guessLevel(line string) slog.Level {
	u := strings.ToUpper(line)
	switch {
	case strings.Contains(u, "ERROR"), strings.Contains(u, "FATAL"), strings.Contains(u, "PANIC"):
		return slog.LevelError
	case strings.Contains(u, "WARN"):
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
