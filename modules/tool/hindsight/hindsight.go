// Package hindsight 是本机自有记忆：把一段文字存下来，按创建时间列出来。
//
// 为什么不让模型自己去写 markdown：记忆要能被**搜到**、能被**按 id 读全文**，
// 这两件事都要求文件名与内容之间有一份稳定映射。让模型自由写 markdown，
// 下一次它就得先 ls 再猜哪个文件是哪个主题，一步变三步。
//
// 边界感：
//   - 只碰 <home>/memory 这一个目录，别处一律不写（Caps.Writes 只给 ingest 声明）
//   - 一个文档一个文件：<home>/memory/<id>.json，整文件写（temp + rename）
//   - 目录不存在是"还没有记忆"，不是错误——第一次 ingest 才创建
package hindsight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

const (
	// maxSlugChars 是 id 里 slug 部分的长度上限（不含 8 位随机十六进制）。
	maxSlugChars = 40
	// randomHexChars 是 id 尾部的随机段长度。
	randomHexChars = 8
	// fileMode / dirMode 与仓库其它落盘工具一致：只给属主写权限。
	fileMode = 0o644
	dirMode  = 0o755
	// suffix 是文档文件扩展名。
	suffix = ".json"
	// idAttempts 是撞 id 时的重试次数（同一秒内同名标题才会用到）。
	idAttempts = 5
)

var ingestSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "这段记忆叫什么，会参与生成 id"},
    "content": {"type": "string", "description": "要记住的正文"},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "可选标签"}
  },
  "required": ["title", "content"],
  "additionalProperties": false
}`)

var listSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

var captureSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "倡议名词，短"},
    "summary": {"type": "string", "description": "两三句：在做什么、为什么"}
  },
  "required": ["title", "summary"],
  "additionalProperties": false
}`)

// document 是磁盘上一份文档的形状。键名就是文件里的键名，不加别的。
type document struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags"`
	Body    string   `json:"body"`
	Created int64    `json:"created"`
}

// ---------------------------------------------------------------------------
// ingest
// ---------------------------------------------------------------------------

type ingestTool struct{ dir string }

func (t *ingestTool) Name() string { return "hindsight_ingest_document" }

func (t *ingestTool) Description() string {
	return "把一段结论/踩坑/约定存进本机记忆，下次能按 id 捞回来；趁还记得时用。"
}

func (t *ingestTool) Schema() json.RawMessage { return ingestSchema }

// Caps: 会建目录、写文件，必须声明，审批与沙箱都按它判定。
func (t *ingestTool) Caps() spi.Caps { return spi.Caps{Writes: true} }

type ingestArgs struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

type ingestData struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (t *ingestTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a ingestArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"title": "…", "content": "…", "tags"?: ["…"]}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		return spi.Result{Hint: "缺 title：这段记忆叫什么？"}, fmt.Errorf("title 为空")
	}
	if strings.TrimSpace(a.Content) == "" {
		return spi.Result{Hint: "缺 content：要记的是哪段内容？"}, fmt.Errorf("content 为空")
	}
	if err := ctx.Err(); err != nil {
		return spi.Result{Hint: "这次调用被取消了；原参数重发一次即可"}, err
	}

	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}

	// 目录由 ingest 创建：读路径遇到不存在只会说"还没有记忆"。
	if err := os.MkdirAll(t.dir, dirMode); err != nil {
		return spi.Result{Hint: "写入 " + slash(t.dir) + " 失败：先确认这个目录可写"},
			fmt.Errorf("创建记忆目录失败: %w", err)
	}

	doc := document{
		Title:   title,
		Tags:    tags,
		Body:    a.Content,
		Created: time.Now().Unix(),
	}
	path, err := t.writeNew(&doc)
	if err != nil {
		return spi.Result{Hint: "写入 " + slash(t.dir) + " 失败：先确认这个目录可写"}, err
	}
	_ = path

	return spi.Result{
		Text:    fmt.Sprintf("已存入记忆「%s」（%s）", doc.Title, doc.ID),
		Summary: fmt.Sprintf("已存入记忆「%s」", doc.Title),
		Data:    ingestData{ID: doc.ID, Title: doc.Title},
	}, nil
}

// writeNew 是 ingest 的方法壳，转发到包级 writeNew（两边落同一套盘逻辑）。
func (t *ingestTool) writeNew(doc *document) (string, error) {
	return writeNew(t.dir, doc)
}

// writeNew 生成不撞车的 id 并原子落盘（同目录 temp + rename）。
func writeNew(dir string, doc *document) (string, error) {
	slug := slugify(doc.Title)
	var lastErr error
	for i := 0; i < idAttempts; i++ {
		suffixHex, err := randomHex(randomHexChars)
		if err != nil {
			return "", fmt.Errorf("生成 id 失败: %w", err)
		}
		id := slug + "-" + suffixHex
		doc.ID = id
		p := filepath.Join(dir, id+suffix)

		// 文件已存在就换一个随机段，绝不覆盖别人的记忆。
		if _, err := os.Stat(p); err == nil {
			lastErr = fmt.Errorf("id 已存在: %s", id)
			continue
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("检查 id 失败: %w", err)
		}

		if err := writeFileAtomic(p, doc); err != nil {
			return "", err
		}
		return p, nil
	}
	return "", fmt.Errorf("连续 %d 次没拿到可用 id: %w", idAttempts, lastErr)
}

// ---------------------------------------------------------------------------
// capture initiative
// ---------------------------------------------------------------------------

// captureTool 把"一个倡议"记成一页：标题 + 两三句说明，标签固定 initiative。
type captureTool struct{ dir string }

func (t *captureTool) Name() string { return "hindsight_capture_initiative" }

func (t *captureTool) Description() string {
	return "把一个倡议记成一页记忆（标题 + 两三句在做什么、为什么）；先记下来，以后再搜。"
}

func (t *captureTool) Schema() json.RawMessage { return captureSchema }

// Caps: 与 ingest 一样会建目录、写文件。
func (t *captureTool) Caps() spi.Caps { return spi.Caps{Writes: true} }

type captureArgs struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

func (t *captureTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a captureArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"title": "…", "summary": "…"}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		return spi.Result{Hint: "缺 title：这个倡议叫什么？"}, fmt.Errorf("title 为空")
	}
	summary := strings.TrimSpace(a.Summary)
	if summary == "" {
		return spi.Result{Hint: "缺 summary：这个倡议在做什么、为什么？"}, fmt.Errorf("summary 为空")
	}
	if err := ctx.Err(); err != nil {
		return spi.Result{Hint: "这次调用被取消了；原参数重发一次即可"}, err
	}

	if err := os.MkdirAll(t.dir, dirMode); err != nil {
		return spi.Result{Hint: "写入 " + slash(t.dir) + " 失败：先确认这个目录可写"},
			fmt.Errorf("创建记忆目录失败: %w", err)
	}

	doc := document{
		Title:   title,
		Tags:    []string{"initiative"},
		Body:    summary,
		Created: time.Now().Unix(),
	}
	if _, err := writeNew(t.dir, &doc); err != nil {
		return spi.Result{Hint: "写入 " + slash(t.dir) + " 失败：先确认这个目录可写"}, err
	}

	return spi.Result{
		Text:    fmt.Sprintf("已建倡议页「%s」（%s）", doc.Title, doc.ID),
		Summary: "已建倡议页 " + doc.ID,
		Data:    map[string]any{"id": doc.ID, "title": doc.Title},
	}, nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

type listTool struct{ dir string }

func (t *listTool) Name() string { return "hindsight_list_knowledge_pages" }

func (t *listTool) Description() string {
	return "列出本机存过的记忆（id、标题、字数，新→旧）；不确定有哪些记忆时先用它。"
}

func (t *listTool) Schema() json.RawMessage { return listSchema }

// Caps 全空：只读目录。
func (t *listTool) Caps() spi.Caps { return spi.Caps{} }

type pageData struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	BodyChars int    `json:"body_chars"`
	Created   int64  `json:"created"`
}

type listData struct {
	Count int        `json:"count"`
	Pages []pageData `json:"pages"`
}

func (t *listTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	// 无参工具仍校验：Schema 是 additionalProperties:false，吃了多余参数就得说出来。
	if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return spi.Result{Hint: "hindsight_list_knowledge_pages 不接受参数：直接发 {} 即可"},
				fmt.Errorf("参数不合法: %w", err)
		}
		if len(probe) > 0 {
			keys := make([]string, 0, len(probe))
			for k := range probe {
				keys = append(keys, k)
			}
			return spi.Result{Hint: "hindsight_list_knowledge_pages 不接受参数（收到 " + strings.Join(keys, ",") + "）：直接发 {} 即可"},
				fmt.Errorf("多余参数: %s", strings.Join(keys, ","))
		}
	}
	if err := ctx.Err(); err != nil {
		return spi.Result{Hint: "这次调用被取消了；原参数重发一次即可"}, err
	}

	docs, err := loadDocs(t.dir)
	if err != nil {
		return spi.Result{Hint: "读 " + slash(t.dir) + " 失败：先确认这个目录可读"}, err
	}
	if len(docs) == 0 {
		return spi.Result{
			Text:    "还没有任何记忆",
			Summary: "0 条记忆",
			Data:    listData{Count: 0, Pages: []pageData{}},
			Hint:    "用 hindsight_ingest_document 存第一条",
		}, nil
	}

	// created 倒序；同一秒内按 id 正序，保证两次调用输出一致（可重放）。
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Created != docs[j].Created {
			return docs[i].Created > docs[j].Created
		}
		return docs[i].ID < docs[j].ID
	})

	var b strings.Builder
	pages := make([]pageData, 0, len(docs))
	for i, d := range docs {
		chars := utf8.RuneCountInString(d.Body)
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s\t%s\t（%d 字）", d.ID, d.Title, chars)
		pages = append(pages, pageData{ID: d.ID, Title: d.Title, BodyChars: chars, Created: d.Created})
	}

	return spi.Result{
		Text:    b.String(),
		Summary: fmt.Sprintf("%d 条记忆", len(docs)),
		Data:    listData{Count: len(docs), Pages: pages},
	}, nil
}

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

var searchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "要搜什么；空格分隔多个词，标题×3 tags×2 正文×1 计分"},
    "limit": {"type": "integer", "description": "最多返回几条；缺省 5，上限 20"}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

const (
	// defaultSearchLimit 是 limit 缺省值；maxSearchLimit 是硬上限。
	defaultSearchLimit = 5
	maxSearchLimit     = 20
	// snippetRunes 是片段向命中位置两侧各取多少个字符。
	snippetRunes = 40
)

type searchTool struct{ dir string }

func (t *searchTool) Name() string { return "hindsight_search_knowledge_pages" }

func (t *searchTool) Description() string {
	return "按关键词搜本机记忆（标题×3 tags×2 正文×1 计分，降序）；记得大概内容但忘了 id 时用它。"
}

func (t *searchTool) Schema() json.RawMessage { return searchSchema }

// Caps 全空：只读目录。
func (t *searchTool) Caps() spi.Caps { return spi.Caps{} }

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type searchHit struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Score   int    `json:"score"`
	Snippet string `json:"snippet,omitempty"`
}

type searchData struct {
	Count int         `json:"count"`
	Query string      `json:"query"`
	Hits  []searchHit `json:"hits"`
}

func (t *searchTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a searchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"query": "…", "limit"?: 5}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return spi.Result{Hint: "缺 query：要搜什么？"}, fmt.Errorf("query 为空")
	}
	if err := ctx.Err(); err != nil {
		return spi.Result{Hint: "这次调用被取消了；原参数重发一次即可"}, err
	}

	limit := a.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	// 分词只做一次：Fields 按空白切，每词小写；空词不可能出现。
	words := strings.Fields(strings.ToLower(query))

	docs, err := loadDocs(t.dir)
	if err != nil {
		return spi.Result{Hint: "读 " + slash(t.dir) + " 失败：先确认这个目录可读"}, err
	}

	type scored struct {
		doc   document
		score int
	}
	ranked := make([]scored, 0, len(docs))
	for _, d := range docs {
		lowTitle := strings.ToLower(d.Title)
		lowBody := strings.ToLower(d.Body)
		lowTags := make([]string, 0, len(d.Tags))
		for _, tag := range d.Tags {
			lowTags = append(lowTags, strings.ToLower(tag))
		}

		titleHits, tagHits, bodyHits := 0, 0, 0
		for _, w := range words {
			titleHits += strings.Count(lowTitle, w)
			bodyHits += strings.Count(lowBody, w)
			for _, tag := range lowTags {
				tagHits += strings.Count(tag, w)
			}
		}
		score := titleHits*3 + tagHits*2 + bodyHits*1
		if score == 0 {
			continue
		}
		ranked = append(ranked, scored{doc: d, score: score})
	}

	// 分数降序；同分按 created 倒序；再同则按 id 正序，保证输出可重放。
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].doc.Created != ranked[j].doc.Created {
			return ranked[i].doc.Created > ranked[j].doc.Created
		}
		return ranked[i].doc.ID < ranked[j].doc.ID
	})

	if len(ranked) == 0 {
		return spi.Result{
			Text:    "没有命中",
			Summary: "命中 0 条",
			Data:    searchData{Count: 0, Query: query, Hits: []searchHit{}},
			Hint:    "换个词；或用 hindsight_list_knowledge_pages 看看都有什么",
		}, nil
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	var b strings.Builder
	hits := make([]searchHit, 0, len(ranked))
	for i, r := range ranked {
		snippet := makeSnippet(r.doc.Body, words)
		if i > 0 {
			b.WriteByte('\n')
		}
		if snippet == "" {
			fmt.Fprintf(&b, "%s\t%s\t%d", r.doc.ID, r.doc.Title, r.score)
		} else {
			fmt.Fprintf(&b, "%s\t%s\t%d\t%s", r.doc.ID, r.doc.Title, r.score, snippet)
		}
		hits = append(hits, searchHit{ID: r.doc.ID, Title: r.doc.Title, Score: r.score, Snippet: snippet})
	}

	return spi.Result{
		Text:    b.String(),
		Summary: fmt.Sprintf("命中 %d 条", len(hits)),
		Data:    searchData{Count: len(hits), Query: query, Hits: hits},
	}, nil
}

// makeSnippet 取正文里**第一个词**首次出现位置前 40 字符到后 40 字符（按 rune 切，
// 中文才不会被切成半个字）。找不到就不带片段；换行/制表符压成空格，保证一条结果占一行。
func makeSnippet(body string, words []string) string {
	if body == "" || len(words) == 0 {
		return ""
	}
	low := strings.ToLower(body)
	for _, w := range words {
		byteIdx := strings.Index(low, w)
		if byteIdx < 0 {
			continue
		}
		runes := []rune(body)
		// byteIdx 落在 low 上，换算成 body 的 rune 下标（ToLower 只对极少数码元改长度，
		// 真改了也只会让片段偏几个字符，不会越界——下面有夹紧）。
		at := utf8.RuneCountInString(low[:byteIdx])
		if at > len(runes) {
			at = len(runes)
		}
		start := at - snippetRunes
		if start < 0 {
			start = 0
		}
		end := at + utf8.RuneCountInString(w) + snippetRunes
		if end > len(runes) {
			end = len(runes)
		}
		return oneLine(string(runes[start:end]))
	}
	return ""
}

// oneLine 把片段里的换行与制表符压成空格：结果按 \t 分列、按行分条，片段不许破坏它。
func oneLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s))
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

var readSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "page_id": {"type": "string", "description": "文档 id，先用 list/search 拿到（DSH 的叫法）"},
    "id": {"type": "string", "description": "同上，另一种叫法；两个都给时以 page_id 为准"}
  },
  "required": ["page_id"],
  "additionalProperties": false
}`)

type readTool struct{ dir string }

func (t *readTool) Name() string { return "hindsight_read_knowledge_page" }

func (t *readTool) Description() string {
	return "按 id 读一份记忆的正文全文；已知 id、要看完整内容时用它。"
}

func (t *readTool) Schema() json.RawMessage { return readSchema }

// Caps 全空：只读目录。
func (t *readTool) Caps() spi.Caps { return spi.Caps{} }

type readArgs struct {
	ID string `json:"id"`
	// PageID 是 DSH 那边用的名字。**两个都收**：只认一个，
	// 照 DSH 契约写的调用方就会拿到"缺参"（2026-11-03 parity 测抓到的）。
	PageID string `json:"page_id"`
}

type readData struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	Created   int64    `json:"created"`
	BodyChars int      `json:"body_chars"`
}

func (t *readTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a readArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"id": "…"}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	id := strings.TrimSpace(a.PageID)
	if id == "" {
		id = strings.TrimSpace(a.ID)
	}
	if id == "" {
		return spi.Result{Hint: "缺 id：先用 hindsight_list_knowledge_pages 看看有哪些"},
			fmt.Errorf("id 为空")
	}
	if err := ctx.Err(); err != nil {
		return spi.Result{Hint: "这次调用被取消了；原参数重发一次即可"}, err
	}

	docs, err := loadDocs(t.dir)
	if err != nil {
		return spi.Result{Hint: "读 " + slash(t.dir) + " 失败：先确认这个目录可读"}, err
	}
	for _, d := range docs {
		if d.ID != id {
			continue
		}
		chars := utf8.RuneCountInString(d.Body)
		tags := d.Tags
		if tags == nil {
			tags = []string{}
		}
		return spi.Result{
			Text:    d.Body,
			Summary: fmt.Sprintf("「%s」（%d 字）", d.Title, chars),
			Data:    readData{ID: d.ID, Title: d.Title, Tags: tags, Created: d.Created, BodyChars: chars},
		}, nil
	}

	return spi.Result{Hint: missingIDHint(id, docs)}, fmt.Errorf("没有 id 为 %s 的记忆", id)
}

// missingIDHint 把"有哪些 id"直接塞进 Hint：模型下一步就靠这句话换成对的 id，
// 不必再花一次调用去 list。最多列 5 个，避免 Hint 本身变长。
func missingIDHint(id string, docs []document) string {
	if len(docs) == 0 {
		return "记忆还是空的：先用 hindsight_ingest_document 存一条"
	}
	// 与 list 一致：created 倒序，同秒按 id，保证同样的输入给同样的提示。
	sorted := make([]document, len(docs))
	copy(sorted, docs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Created != sorted[j].Created {
			return sorted[i].Created > sorted[j].Created
		}
		return sorted[i].ID < sorted[j].ID
	})
	const shown = 5
	ids := make([]string, 0, shown)
	for i, d := range sorted {
		if i >= shown {
			break
		}
		ids = append(ids, d.ID)
	}
	hint := "没有 id「" + id + "」；已有的 id：" + strings.Join(ids, ", ")
	if len(sorted) > shown {
		hint += fmt.Sprintf("（共 %d 条，用 hindsight_list_knowledge_pages 看全）", len(sorted))
	}
	return hint
}

// ---------------------------------------------------------------------------
// 存储
// ---------------------------------------------------------------------------

// loadDocs 读目录里的全部文档。目录（或 home）不存在 → 空列表，不是错误。
// 单独一个坏文件不拖垮整张单子：它不是我们能解释的文档，跳过。
func loadDocs(dir string) ([]document, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	docs := make([]document, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var d document
		if err := json.Unmarshal(b, &d); err != nil {
			continue
		}
		if d.ID == "" || d.Title == "" {
			continue
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// writeFileAtomic 先写同目录临时文件再 rename：中途失败不会留下半个 json。
func writeFileAtomic(path string, doc *document) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("序列化文档失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, fileMode); err != nil {
		return fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("提交文档失败: %w", err)
	}
	return nil
}

// slugify：小写、非 [a-z0-9] 一律换成 '-'、连续 '-' 折叠、去首尾 '-'、截到 40。
// 全中文标题会退化成空串，此时用 "memory" 兜底——id 不能只剩随机段。
func slugify(title string) string {
	s := strings.ToLower(title)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxSlugChars {
		out = strings.TrimRight(out[:maxSlugChars], "-")
	}
	if out == "" {
		return "memory"
	}
	return out
}

// randomHex 返回 n 位小写十六进制。id 尾部用它，避免同标题覆盖。
func randomHex(n int) (string, error) {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf)[:n], nil
}

// slash 把路径统一成正斜杠再交给模型（反斜杠会被误读成转义）。
func slash(p string) string { return filepath.ToSlash(p) }

// Module 返回装配用的模块描述。
func Module(home string) kernel.Module {
	return kernel.Module{
		ID:       "tool-hindsight",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			dir := filepath.Join(home, "memory")
			for _, t := range []spi.Tool{
				&ingestTool{dir: dir},
				&listTool{dir: dir},
				&searchTool{dir: dir},
				&readTool{dir: dir},
				&captureTool{dir: dir},
			} {
				if err := reg.Register(t); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
