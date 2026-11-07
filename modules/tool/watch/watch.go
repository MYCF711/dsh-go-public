// Package watch 提供四个工具，登记"要盯哪些 GitHub 仓库"并记账"已处理到第几个编号"。
//
// 本包**不联网、不调 gh**：真正去查 issue/PR 的是宿主的 `dshd watch check`。
// 这里只负责把名单和 watermark 落到一张小表上——分开的理由是，查一次要几十秒，
// 而模型每次思考都会碰一次名单，两者的生命周期和失败方式完全不同。
//
// 存储是一个 JSON 文件（watchers.json）：出事时人能直接打开看懂并改好。
// 四个人工具共用一个 Tool 类型、按 kind 分派，因为它们操作的是同一份状态；
// 拆开只会把 load/save 抄四遍。
package watch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

const fileName = "watchers.json"

// 四个工具的 kind，同时也是模型看到的工具名。
const (
	kindStart  = "watch_start"
	kindStatus = "watch_status"
	kindStop   = "watch_stop"
	kindAck    = "watch_ack"
	kindDoctor = "watch_doctor"
)

const (
	defaultAckLabel = "已处理"
	// 输出有界：名单通常只有几条，但坏文件可能塞进来上千条。
	maxListItems = 200
)

// Watcher 是名单里的一条。字段名就是落盘格式，改之前想想盘上的旧文件。
type Watcher struct {
	ID           string   `json:"id"`
	Repo         string   `json:"repo"` // owner/name，落盘时保留用户写的大小写
	Labels       []string `json:"labels"`
	IgnoreLabels []string `json:"ignore_labels"`
	OnlyAssigned bool     `json:"only_assigned"`
	AckLabel     string   `json:"ack_label"`
	Watermark    int      `json:"watermark"` // 已处理到的最大编号
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

type watcherFile struct {
	Watchers []Watcher `json:"watchers"`
}

// store 是四个工具共享的存储句柄。进程内互斥即可：
// 单进程里并发写同一个 JSON 才是真正的危险，跨进程竞争不在本设计承诺内。
type store struct {
	mu  sync.Mutex
	dir string
}

func (s *store) path() string { return filepath.Join(s.dir, fileName) }

// display 只用于提示：对外一律正斜杠，Windows 的反斜杠不喂给模型。
func (s *store) display() string { return strings.ReplaceAll(s.path(), `\`, "/") }

func (s *store) ready() error {
	if strings.TrimSpace(s.dir) == "" {
		return errors.New("watch: 没有配置状态目录")
	}
	return nil
}

// load 读出全部登记；文件不存在或为空都当作空名单（第一天上工不该报错）。
// 调用方必须已持锁。
func (s *store) load() ([]Watcher, error) {
	buf, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读名单 %s 失败: %w", s.display(), err)
	}
	if len(strings.TrimSpace(string(buf))) == 0 {
		return nil, nil
	}
	var f watcherFile
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("名单 %s 不是合法 JSON: %w", s.display(), err)
	}
	return f.Watchers, nil
}

// save 先写 .tmp 再 rename：崩在中间时旧文件仍然完整，不会留下半截 JSON。
// 调用方必须已持锁。
func (s *store) save(list []Watcher) error {
	if list == nil {
		list = []Watcher{}
	}
	buf, err := json.MarshalIndent(watcherFile{Watchers: list}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化名单失败: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("建状态目录 %s 失败: %w", s.display(), err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写临时文件 %s.tmp 失败: %w", s.display(), err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		os.Remove(tmp) // 留个 .tmp 会让人以为还有一次没跑完的写
		return fmt.Errorf("替换名单 %s 失败: %w", s.display(), err)
	}
	return nil
}

// ---------------------------------------------------------------- 工具

// Tool 的四个形态共用一个 struct，靠 kind 分派；这样 Module() 只需注册四次。
type Tool struct {
	st   *store
	kind string
}

func (t *Tool) Name() string { return t.kind }

// Description 只讲"什么时候用"，不讲实现；<=120 字由 desc-len.py 把关。
// 注意脚本把函数体里的字面量**累加**，所以四段合起来也得压住。
func (t *Tool) Description() string {
	switch t.kind {
	case kindStart:
		return "要开始盯一个新仓库时用它登记，之后由 dshd watch check 轮询。"
	case kindStatus:
		return "看当前在盯哪些仓库、各自已处理到第几个编号。"
	case kindStop:
		return "不再盯某个仓库时把它从名单里摘掉。"
	case kindDoctor:
		return "体检：gh 登录、仓库权限、打标标签是否存在。登记完先跑它，别等轮询时才炸。"
	default:
		return "打完标后调它，把已处理水位推进到这批编号的最大值。"
	}
}

func (t *Tool) Schema() json.RawMessage {
	switch t.kind {
	case kindStart:
		return json.RawMessage(`{"type":"object","properties":{
  "repo":{"type":"string","description":"要盯的仓库，形如 owner/name"},
  "labels":{"type":"array","items":{"type":"string"},"description":"可选：只盯带这些标签的 issue/PR"},
  "ignore_labels":{"type":"array","items":{"type":"string"},"description":"可选：带这些标签的一律跳过"},
  "only_assigned":{"type":"boolean","description":"可选：只看指派给我的"},
  "ack_label":{"type":"string","description":"可选：处理完要打的标签，默认「已处理」"}
},"additionalProperties":false}`)
	case kindStatus:
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	case kindStop:
		return json.RawMessage(`{"type":"object","properties":{
  "repo":{"type":"string","description":"要停掉的仓库，形如 owner/name"}
},"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{
  "repo":{"type":"string","description":"要记帐的仓库，形如 owner/name"},
  "numbers":{"type":"array","items":{"type":"integer"},"description":"已处理完的 issue/PR 编号"},
  "confirm_skip":{"type":"boolean",
                  "description":"跳过的编号超过 3 个时必须显式确认：水位跳过去的编号以后不会再被处理"}
},"required":["numbers"],"additionalProperties":false}`)
	}
}

// Caps：status/doctor 只读文件；另外三个会写名单。
//
// doctor 虽然不写盘，但它**起 gh 子进程、而 gh 要联网**——按 AGENTS §2
// 不能因为"只是查一下"就漏报能力，否则 deny 策略下它会静默放行一次联网调用。
func (t *Tool) Caps() spi.Caps {
	switch t.kind {
	case kindStatus:
		return spi.Caps{}
	case kindDoctor:
		return spi.Caps{Executes: true, Network: true}
	}
	return spi.Caps{Writes: true}
}

type startArgs struct {
	Repo         string   `json:"repo"`
	Labels       []string `json:"labels"`
	IgnoreLabels []string `json:"ignore_labels"`
	OnlyAssigned bool     `json:"only_assigned"`
	AckLabel     string   `json:"ack_label"`
}

type stopArgs struct {
	Repo string `json:"repo"`
}

type ackArgs struct {
	Repo    string `json:"repo"`
	Numbers []int  `json:"numbers"`
	// ConfirmSkip 用于显式承认"这一跳会漏掉一批编号"。
	// 小批跳号（<= maxSilentSkip）不问；大批必须承认一次。
	ConfirmSkip bool `json:"confirm_skip"`
}

// maxSilentSkip 是"可以直接跳过去"的编号个数上限。
//
// 定 3 的理由：正常批量记账很少连号且中间空两三个以上；
// 跳过 4 个以上通常在说明"我一次性把范围推到了很后面"——
// 那正是最容易把没处理的东西说成处理完的动作。
const maxSilentSkip = 3

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	switch t.kind {
	case kindStart:
		return t.start(ctx, raw)
	case kindStatus:
		return t.status(ctx, raw)
	case kindStop:
		return t.stop(ctx, raw)
	case kindDoctor:
		return t.doctor(ctx)
	default:
		return t.ack(ctx, raw)
	}
}

// ---------------------------------------------------------------- watch_start

func (t *Tool) start(_ context.Context, raw json.RawMessage) (spi.Result, error) {
	var a startArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：至少给 {"repo":"owner/name"}，其余字段都可省略`}, err
	}
	repo, err := t.resolveRepo(a.Repo)
	if err != nil {
		return spi.Result{Hint: `repo 要写成 owner/name；只盯了一个仓库时可以省略，多了就必须指定`}, err
	}
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "状态目录没配上：这是接线问题，报告监工，别改配置自己试"}, err
	}

	label := strings.TrimSpace(a.AckLabel)
	if label == "" {
		label = defaultAckLabel
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()

	list, err := t.st.load()
	if err != nil {
		return spi.Result{Hint: fmt.Sprintf("名单读不出来：看一眼 %s 是不是被手改坏了，改回合法 JSON 或删掉它重新登记", t.st.display())}, err
	}
	if i := indexOf(list, repo); i >= 0 {
		w := list[i]
		return spi.Result{Hint: fmt.Sprintf(`%s 已经在盯了（%s，水位 %d）：先 watch_status 看，要改就先 watch_stop`, w.Repo, w.ID, w.Watermark)},
			fmt.Errorf("watch: %s 已登记", w.Repo)
	}

	id, err := nextID(list)
	if err != nil {
		return spi.Result{Hint: "生成 id 失败（系统随机源不可用）：重试一次；连续失败就报告监工"}, err
	}
	now := nowStamp()
	w := Watcher{
		ID:           id,
		Repo:         repo,
		Labels:       cleanList(a.Labels),
		IgnoreLabels: cleanList(a.IgnoreLabels),
		OnlyAssigned: a.OnlyAssigned,
		AckLabel:     label,
		Watermark:    0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	list = append(list, w)
	if err := t.st.save(list); err != nil {
		return spi.Result{Hint: fmt.Sprintf("没落盘：检查 %s 的权限和磁盘空间，重试一次；别当作已登记", t.st.display())}, err
	}

	return spi.Result{
		Summary: fmt.Sprintf("已开始盯 %s（%s）", w.Repo, w.ID),
		Data: map[string]any{
			"action":  "start",
			"watcher": w,
			"total":   len(list),
		},
		Hint: "登记完不会立刻拉数据：要等宿主跑一次 `dshd watch check`",
	}, nil
}

// ---------------------------------------------------------------- watch_status

func (t *Tool) status(_ context.Context, _ json.RawMessage) (spi.Result, error) {
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "状态目录没配上：这是接线问题，报告监工，别改配置自己试"}, err
	}

	t.st.mu.Lock()
	list, err := t.st.load()
	t.st.mu.Unlock()
	if err != nil {
		return spi.Result{Hint: fmt.Sprintf("名单读不出来：看一眼 %s 是不是被手改坏了，改回合法 JSON 或删掉它重新登记", t.st.display())}, err
	}

	if len(list) == 0 {
		return spi.Result{
			Summary: "还没有在盯任何仓库",
			Text:    "还没有在盯任何仓库。",
			Data:    map[string]any{"action": "status", "total": 0, "watchers": []Watcher{}},
			Hint:    `要开始盯的话用 watch_start，例如 {"repo":"owner/name"}`,
		}, nil
	}

	shown := list
	if len(shown) > maxListItems {
		shown = shown[:maxListItems]
	}
	var b strings.Builder
	for _, w := range shown {
		fmt.Fprintf(&b, "%s · 水位 %d · 建于 %s · 打标 %s\n", w.Repo, w.Watermark, w.CreatedAt, w.AckLabel)
	}
	text := strings.TrimRight(b.String(), "\n")
	if len(list) > len(shown) {
		text += fmt.Sprintf("\n（还有 %d 条没列出）", len(list)-len(shown))
	}

	return spi.Result{
		Summary: fmt.Sprintf("在盯 %d 个仓库", len(list)),
		Text:    text,
		Data: map[string]any{
			"action":   "status",
			"total":    len(list),
			"watchers": shown,
		},
		Hint: "水位是「已处理到的最大编号」：比它大的才是新活；要改配置先 watch_stop",
	}, nil
}

// ---------------------------------------------------------------- watch_stop

// resolveRepo 处理"只有一个仓库在盯时不填 repo"的情形。
//
// 让模型填一个它刚登记过的 owner/name，只是多一次被拒的机会；
// 但**有多个**时必须让模型选，猜错就记到别人头上了。
func (t *Tool) resolveRepo(repo string) (string, error) {
	if strings.TrimSpace(repo) != "" {
		return normRepo(repo)
	}
	list, err := t.st.load()
	if err != nil {
		return "", err
	}
	switch len(list) {
	case 0:
		return "", fmt.Errorf("还没有在盯任何仓库：先 watch_start 登记一个")
	case 1:
		return list[0].Repo, nil
	default:
		names := make([]string, 0, len(list))
		for _, w := range list {
			names = append(names, w.Repo)
		}
		return "", fmt.Errorf("盯了多个仓库（%s），repo 必须填一个", strings.Join(names, "、"))
	}
}

func (t *Tool) stop(_ context.Context, raw json.RawMessage) (spi.Result, error) {
	var a stopArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"repo":"owner/name"}`}, err
	}
	repo, err := normRepo(a.Repo)
	if err != nil {
		return spi.Result{Hint: `repo 要写成 owner/name：一条斜杠、两边都不空，例如 "dshgo/dshd"`}, err
	}
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "状态目录没配上：这是接线问题，报告监工，别改配置自己试"}, err
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()

	list, err := t.st.load()
	if err != nil {
		return spi.Result{Hint: fmt.Sprintf("名单读不出来：看一眼 %s 是不是被手改坏了，改回合法 JSON 或删掉它重新登记", t.st.display())}, err
	}
	i := indexOf(list, repo)
	if i < 0 {
		have := repoList(list)
		if have == "" {
			return spi.Result{Hint: "名单是空的，没有可停的；要开始盯就用 watch_start"}, fmt.Errorf("watch: %s 不在名单里", repo)
		}
		return spi.Result{Hint: fmt.Sprintf("%s 不在名单里；当前在盯：%s（用 watch_status 看细节，repo 要完全对上）", repo, have)},
			fmt.Errorf("watch: %s 不在名单里", repo)
	}

	gone := list[i]
	list = append(list[:i], list[i+1:]...)
	if err := t.st.save(list); err != nil {
		return spi.Result{Hint: fmt.Sprintf("没落盘：检查 %s 的权限和磁盘空间，重试一次；别当作已停", t.st.display())}, err
	}

	return spi.Result{
		Summary: fmt.Sprintf("已停止盯 %s（%s，水位停在 %d）", gone.Repo, gone.ID, gone.Watermark),
		Data: map[string]any{
			"action":     "stop",
			"repo":       gone.Repo,
			"removed_id": gone.ID,
			"total":      len(list),
		},
		Hint: "水位随登记一起丢弃：再想盯同一个仓库要重新 watch_start，会从头开始数",
	}, nil
}

// ---------------------------------------------------------------- watch_ack

// spanText 把一串连续编号压成人能读的短形式：4,5,6,9 → "4-6、9"。
func spanText(nums []int) string {
	if len(nums) == 0 {
		return ""
	}
	var parts []string
	start, prev := nums[0], nums[0]
	flush := func() {
		if start == prev {
			parts = append(parts, fmt.Sprintf("%d", start))
			return
		}
		parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
	}
	for _, n := range nums[1:] {
		if n == prev+1 {
			prev = n
			continue
		}
		flush()
		start, prev = n, n
	}
	flush()
	return strings.Join(parts, "、")
}

func (t *Tool) ack(_ context.Context, raw json.RawMessage) (spi.Result, error) {
	var a ackArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"repo":"owner/name","numbers":[12,13]}`}, err
	}
	repo, err := t.resolveRepo(a.Repo)
	if err != nil {
		return spi.Result{Hint: `repo 要写成 owner/name；只盯了一个仓库时可以省略，多了就必须指定`}, err
	}

	// 只收正数：issue/PR 编号从 1 开始，0 和负数只可能是抄错了。
	nums := make([]int, 0, len(a.Numbers))
	for _, n := range a.Numbers {
		if n > 0 {
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return spi.Result{Hint: "numbers 里没有正的编号：issue/PR 编号从 1 开始，填至少一个真实编号；只想看水位就用 watch_status"},
			fmt.Errorf("watch: numbers 为空或不含正数")
	}
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "状态目录没配上：这是接线问题，报告监工，别改配置自己试"}, err
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()

	list, err := t.st.load()
	if err != nil {
		return spi.Result{Hint: fmt.Sprintf("名单读不出来：看一眼 %s 是不是被手改坏了，改回合法 JSON 或删掉它重新登记", t.st.display())}, err
	}
	i := indexOf(list, repo)
	if i < 0 {
		have := repoList(list)
		if have == "" {
			return spi.Result{Hint: "还没在盯任何仓库，没有可记账的；先 watch_start 登记"}, fmt.Errorf("watch: %s 不在名单里", repo)
		}
		return spi.Result{Hint: fmt.Sprintf("%s 不在名单里；当前在盯：%s（用 watch_status 核对 repo 写法）", repo, have)},
			fmt.Errorf("watch: %s 不在名单里", repo)
	}

	w := list[i]
	prev := w.Watermark
	top := 0
	for _, n := range nums {
		if n > top {
			top = n
		}
	}
	// 一次 ack [15] 会静默吞掉 4..14 —— 这比多处理一批更难发现，
	// 所以把"跳过了哪些"算出来，写在人看的那一行里。
	acked := make(map[int]bool, len(nums))
	for _, n := range nums {
		acked[n] = true
	}
	// 注意用 top 而不是 w.Watermark：这段挪到了落盘**之前**，
	// 那时 w.Watermark 还是旧值，写成 w.Watermark 会让循环一次都不跑。
	var skipped []int
	for n := prev + 1; n <= top; n++ {
		if !acked[n] {
			skipped = append(skipped, n)
		}
	}

	// 跳太多就停下来问一次：水位跳过去的编号**永远不会再被处理**，
	// 而这个动作不会报错、不会重试，下轮也不再提。报告不够——要拦。
	if len(skipped) > maxSilentSkip && !a.ConfirmSkip {
		return spi.Result{Hint: fmt.Sprintf(
				"这次会把水位从 %d 推到 %d，跳过 %d 个编号（%s）——它们以后不会再被当成新活。"+
					"要么把这批编号也列进 numbers，要么带上 confirm_skip=true 明确认下这个后果",
				prev, top, len(skipped), spanText(skipped))},
			fmt.Errorf("watch_ack: 跳过了 %d 个编号，需要 confirm_skip", len(skipped))
	}

	// 只增不减：水位回退会让同一批 issue 被反复当成新活。
	advanced := top > prev
	if advanced {
		w.Watermark = top
		w.UpdatedAt = nowStamp()
		list[i] = w
		if err := t.st.save(list); err != nil {
			return spi.Result{Hint: fmt.Sprintf("没落盘：检查 %s 的权限和磁盘空间，重试一次；别当作已记账，否则下一轮会重复报这批", t.st.display())}, err
		}
	}

	summary := fmt.Sprintf("%s 水位 %d → %d", w.Repo, prev, w.Watermark)
	if len(skipped) > 0 {
		summary += fmt.Sprintf("（跳过 %d 个编号：%s）", len(skipped), spanText(skipped))
	}
	res := spi.Result{
		Summary: summary,
		Data: map[string]any{
			"action":             "ack",
			"repo":               w.Repo,
			"numbers":            nums,
			"watermark":          w.Watermark,
			"previous_watermark": prev,
			"advanced":           advanced,
			"skipped":            skipped,
			"skipped_count":      len(skipped),
		},
	}
	if len(skipped) > 0 {
		res.Hint = fmt.Sprintf("水位跳过了 %s：这些编号以后不会再被当成新活。"+
			"如果它们还没处理，现在就是最后的机会（把水位调回来要手改 %s）", spanText(skipped), t.st.display())
	}
	if !advanced {
		res.Hint = fmt.Sprintf("这批编号都不超过现有水位 %d：可能已经记过账，不用重试；真要往前推，先确认编号没抄错", prev)
	}
	return res, nil
}

// ---------------------------------------------------------------- 公共小工具

// normRepo 校验并规整 owner/name：只去空白，不改大小写（人看得出自己写的样子）。
func normRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("watch: repo 为空")
	}
	if !strings.Contains(s, "/") {
		return "", fmt.Errorf("watch: repo %q 不含斜杠", s)
	}
	parts := strings.SplitN(s, "/", 2)
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return "", fmt.Errorf("watch: repo %q 斜杠两边有空项", s)
	}
	if strings.ContainsAny(owner+name, " \t\r\n") {
		return "", fmt.Errorf("watch: repo %q 含空白", s)
	}
	return owner + "/" + name, nil
}

// indexOf 按大小写不敏感比较：GitHub 的 owner/name 本身不分大小写，
// 否则 DSH-Go/X 与 dsh-go/x 会被登记两遍、各记各的水位。
func indexOf(list []Watcher, repo string) int {
	for i := range list {
		if strings.EqualFold(list[i].Repo, repo) {
			return i
		}
	}
	return -1
}

// repoList 给"找不到"的提示用：直接把现成的写出来，省模型一轮 watch_status。
func repoList(list []Watcher) string {
	if len(list) == 0 {
		return ""
	}
	out := make([]string, 0, len(list))
	for i, w := range list {
		if i == maxListItems {
			out = append(out, fmt.Sprintf("等 %d 个", len(list)))
			break
		}
		out = append(out, w.Repo)
	}
	return strings.Join(out, "、")
}

// cleanList 去掉空白与重复，保住顺序——标签顺序对读的人有意义。
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

func idSeq(id string) int {
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return 0
	}
	n := 0
	for _, ch := range parts[1] {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// nextID 生成 watch-0001-a1b2：序号取名单最大值 +1，后缀是随机 4 位十六进制。
// 不用 UUID：这个 id 是给人念、给人贴进对话里的，短才有用。
func nextID(list []Watcher) (string, error) {
	max := 0
	for _, w := range list {
		if n := idSeq(w.ID); n > max {
			max = n
		}
	}
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成 watch id 失败: %w", err)
	}
	return fmt.Sprintf("watch-%04d-%s", max+1, hex.EncodeToString(buf[:])), nil
}

// Module 返回装配用的模块描述。dir 是状态目录（接线时传 <home>/state/watch）。
func Module(dir string) kernel.Module {
	st := &store{dir: dir}
	return kernel.Module{
		ID:       "tool-watch",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			for _, kind := range []string{kindStart, kindStatus, kindStop, kindAck, kindDoctor} {
				if err := reg.Register(&Tool{st: st, kind: kind}); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
