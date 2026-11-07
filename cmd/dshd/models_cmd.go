package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"dshgo/internal/llm"
)

// cmdModels 问网关有哪些模型。
//
// 为什么要有这条：模型 id 写错/上游坏掉时的现象是**模型一声不吭**——
// "空响应"这种错误里如果不给一条能自己验证的命令，用户只能靠猜。
// 这条命令就是那句话的落点：`--model` 该填什么，看一眼就知道。
//
// 同时把"能不能连上"和"有哪些"分开报：连不上是网关/凭据的问题，
// 连上了但列表是空的，是上游没挂模型——两件事的下一步完全不同。
func cmdModels(argv []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	base := fs.String("base", envOr("DSHD_LLM_BASE", "http://127.0.0.1:3425/v1"), "OpenAI 兼容端点")
	timeout := fs.Duration("timeout", 15*time.Second, "等多久算超时")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	c := llm.New(*base, os.Getenv("DSHD_LLM_KEY"))
	list, err := c.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("问 %s 的模型列表失败: %w\n"+
			"→ 这一步不通说明是网关/凭据的问题（不是模型 id 写错）：先 dshd doctor 看端点，再确认 DSHD_LLM_KEY", *base, err)
	}
	if len(list) == 0 {
		return fmt.Errorf("%s 通了，但它一个模型都没报：往上挂一个再试", *base)
	}
	fmt.Printf("%s 报了 %d 个模型：\n", *base, len(list))
	for _, m := range list {
		if m.OwnedBy != "" {
			fmt.Printf("  %-34s (%s)\n", m.ID, m.OwnedBy)
			continue
		}
		fmt.Printf("  %s\n", m.ID)
	}
	fmt.Printf("\n用法：dshd run --model <上面任意一个> …\n")
	return nil
}
