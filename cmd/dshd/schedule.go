package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dshgo/internal/schedule"
)

// cmdSchedule 让登记过的定时任务真的"到期"。
//
// 用法：
//
//	dshd schedule list  [--home X]             看所有会话的定时任务
//	dshd schedule due   [--home X] [--session] [--advance]
//
// 设计取舍：**不做常驻 ticker**。
// `run` 是一次性的，`acp` 虽然长连但没有"宿主主动发起一轮对话"的通道，
// 硬起 goroutine 只会得到一个自己调模型、却没人能插话的东西。
// 所以这里只回答"现在谁到期了"，并把 next_at 推到下一次；
// 谁来处理（cron、shell 循环、以后的客户端）由调用方决定。
func cmdSchedule(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: dshd schedule list|due|run [--home <dir>]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("schedule "+sub, flag.ContinueOnError)
	home := fs.String("home", defaultHome(), "会话根目录")
	only := fs.String("session", "", "只看这个会话")
	advance := fs.Bool("advance", false, "把到期的推进到下一次（不写就不动）")
	// run 子命令用：跑一轮时的审批策略与步数上限，透传给 cmdRun。
	// 定时任务是无人值守的：默认 allow，否则每个任务都会死在"没人点同意"上。
	// 这是**有意放宽**，所以每次真跑都要把它打在屏幕上（见下面 run 分支）。
	policy := fs.String("policy", "allow", "schedule run 的审批策略：ask/allow/deny/auto")
	maxSteps := fs.Int("max-steps", 12, "schedule run 每轮最多几步")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dir := filepath.Join(*home, "state", "schedules")

	sessions, err := schedule.Sessions(dir)
	if err != nil {
		return fmt.Errorf("读定时任务目录失败（%s）: %w", dir, err)
	}
	if *only != "" {
		sessions = []string{*only}
	}

	switch sub {
	case "list":
		total := 0
		for _, sid := range sessions {
			items, err := schedule.Load(dir, sid)
			if err != nil {
				fmt.Printf("  ! %s: %v\n", sid, err)
				continue
			}
			for _, it := range items {
				total++
				fmt.Printf("%s  %-10s %-22s %s\n", shortID(sid), it.Kind, it.Title, it.NextAt)
			}
		}
		if total == 0 {
			fmt.Println("还没有定时任务（用 schedule_create 建一个）")
			return nil
		}
		fmt.Printf("共 %d 条\n", total)
		return nil

	case "run":
		// 把到期的任务真的变成一轮对话。
		// 复用 cmdRun（同一个进程内），而不是另写一套：审批、压缩、日志、落盘全都一致。
		now := time.Now()
		var ran int
		for _, sid := range sessions {
			items, err := schedule.Load(dir, sid)
			if err != nil {
				fmt.Printf("  ! %s: %v\n", shortID(sid), err)
				continue
			}
			dues := schedule.DueAt(items, now)
			for _, d := range dues {
				itemPolicy, note := schedulePolicy(d.Item.AllowWrites, *policy)
				fmt.Printf("跑 %s  %s %s\n", shortID(sid), d.Item.Title, note)
				args := []string{"--home", *home, "--session", sid, "--prompt", d.Item.Prompt,
					"--policy", itemPolicy, "--max-steps", strconv.Itoa(*maxSteps), "--agents", "docs/WORKER.md"}
				if err := cmdRun(args); err != nil {
					fmt.Printf("  ! 这一轮失败: %v\n", err)
					continue
				}
				ran++
			}
			if len(dues) > 0 {
				if n, err := schedule.Advance(dir, sid, dues, now); err != nil {
					fmt.Printf("  ! 推进失败 %s: %v\n", shortID(sid), err)
				} else {
					fmt.Printf("  已推进 %d 条\n", n)
				}
			}
		}
		if ran == 0 {
			fmt.Println("现在没有到期的任务")
		} else {
			// 把"刚才那些轮次没有经过审批"说出来。定时任务的授权是登记时给的，
			// 跑的时候人不在场——不提示，用户就不知道写操作被自动放行了。
			fmt.Printf("注意：这 %d 轮用的是 --policy=%s（无人值守，写操作没有逐次询问）；要改成别的就带 --policy\n", ran, *policy)
		}
		return nil

	case "due":
		now := time.Now()
		var fired int
		for _, sid := range sessions {
			items, err := schedule.Load(dir, sid)
			if err != nil {
				fmt.Printf("  ! %s: %v\n", sid, err)
				continue
			}
			dues := schedule.DueAt(items, now)
			if len(dues) == 0 {
				continue
			}
			for _, d := range dues {
				fired++
				fmt.Printf("到期 %s  %s\n  提示: %s\n", shortID(sid), d.Item.Title, clipLine(d.Item.Prompt))
			}
			if *advance {
				if n, err := schedule.Advance(dir, sid, dues, now); err != nil {
					fmt.Printf("  ! 推进失败 %s: %v\n", shortID(sid), err)
				} else {
					fmt.Printf("  已推进 %d 条（下次不会再触发同一次）\n", n)
				}
			}
		}
		if fired == 0 {
			fmt.Println("现在没有到期的任务")
			return nil
		}
		if !*advance {
			fmt.Println("（只看了没动；加 --advance 才会把 next_at 推到下一次）")
		}
		return nil
	}
	return fmt.Errorf("未知子命令 %q：只有 list / due / run", sub)
}

func shortID(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:8] + "…" + s[len(s)-4:]
}

func clipLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
