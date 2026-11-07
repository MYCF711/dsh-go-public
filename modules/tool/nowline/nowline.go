// Package nowline 提供 `now_line` 工具：一次报告本地时间、日期、时区与星期。
//
// 为什么需要它：模型几乎每次要"现在几点/今天几号"都会去跑 `bash date`，
// 再自己切字符串猜时区偏移——一步工具调用换三次解析。这里一次给全，
// 且带时区偏移（RFC3339），跨时区对话时不会被误读成 UTC。
//
// 边界感：
//   - 只读本地时钟，无副作用：Caps 全空，不触发审批
//   - 星期用中文，因为 Summary 是给人看的
//   - 无参数；模型臆造参数当场说清，别让它以为参数生效了
package nowline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// weekdayCN 用表而不是 time.Weekday.String()：后者是英文缩写，Summary 要给人读。
var weekdayCN = [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

type Tool struct{}

func (t *Tool) Name() string { return "now_line" }

// Description 只讲"什么时候用"，不讲实现。
func (t *Tool) Description() string {
	return "当前时间、日期、时区名与星期，一次拿全；别为这个跑 bash date。"
}

func (t *Tool) Schema() json.RawMessage { return schema }

// Caps 全空：只读时钟，不写盘、不起进程、不联网。
func (t *Tool) Caps() spi.Caps { return spi.Caps{} }

// data 是成功时的结构化载荷，键名 snake_case。
type data struct {
	RFC3339  string `json:"rfc3339"`
	Timezone string `json:"timezone"`
	Weekday  string `json:"weekday"`
	Unix     int64  `json:"unix"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	// 无参数工具仍校验一次：Schema 是 additionalProperties:false，
	// 传了东西就说清楚，否则模型会以为参数被采纳了。
	if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return spi.Result{Hint: `now_line 不接受参数：直接发 {} 即可`},
				fmt.Errorf("参数不合法: %w", err)
		}
		if len(probe) > 0 {
			keys := make([]string, 0, len(probe))
			for k := range probe {
				keys = append(keys, k)
			}
			return spi.Result{Hint: `now_line 不接受参数（收到 ` + strings.Join(keys, ",") + `）：直接发 {} 即可`},
				fmt.Errorf("多余参数: %s", strings.Join(keys, ","))
		}
	}

	now := time.Now()
	loc := now.Location()

	// timezone 取 Location().String()：本机常见是 "Local" / "Asia/Shanghai" / "UTC"，
	// 模型据此判断偏移含义，比只给 "+08:00" 信息多。
	tz := loc.String()

	d := data{
		RFC3339:  now.Format(time.RFC3339),
		Timezone: tz,
		Weekday:  weekdayCN[int(now.Weekday())],
		Unix:     now.Unix(),
	}

	return spi.Result{
		Summary: now.Format("2006-01-02 15:04:05") + " " + d.Weekday,
		Data:    d,
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-nowline",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{})
		},
	}
}
