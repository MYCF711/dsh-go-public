package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
	execbash "dshgo/modules/exec/bash"
)

type integrationJob struct {
	mu          sync.Mutex
	out         spi.JobOutput
	outputErr   error
	outputCalls int
	killCalls   int
}

func (j *integrationJob) Output(id string, wait time.Duration) (spi.JobOutput, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.outputCalls++
	if j.outputErr != nil {
		return spi.JobOutput{}, j.outputErr
	}
	return j.out, nil
}

func (j *integrationJob) Kill(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.killCalls++
	if j.outputErr != nil {
		return j.outputErr
	}
	j.out.Done = true
	j.out.ExitCode = -1
	return nil
}

func (j *integrationJob) List() []spi.JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	return []spi.JobInfo{{ID: j.out.ID, Done: j.out.Done, ExitCode: j.out.ExitCode}}
}

func (j *integrationJob) counts() (output, kill int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.outputCalls, j.killCalls
}

type integrationPager struct {
	*integrationJob
	retainedOffset int64
	producedTotal  int64
	maxPageBytes   int
	pageCalls      int
}

func (j *integrationPager) OutputPage(id string, cursor int64, limit int, wait time.Duration) (spi.JobOutputPage, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pageCalls++
	if j.outputErr != nil {
		return spi.JobOutputPage{}, j.outputErr
	}
	total := j.producedTotal
	if total == 0 {
		total = j.retainedOffset + int64(len(j.out.Output))
	}
	if cursor < 0 || cursor > total {
		return spi.JobOutputPage{}, spi.ErrJobInvalidCursor
	}
	offset := cursor
	if offset < j.retainedOffset {
		offset = j.retainedOffset
	}
	start := offset - j.retainedOffset
	if start < 0 || start > int64(len(j.out.Output)) {
		return spi.JobOutputPage{}, fmt.Errorf("测试 pager 的保留数据与 total 不一致: offset=%d base=%d bytes=%d", offset, j.retainedOffset, len(j.out.Output))
	}
	if limit <= 0 {
		limit = spi.JobDefaultPageLimit
	}
	if j.maxPageBytes > 0 && limit > j.maxPageBytes {
		limit = j.maxPageBytes
	}
	end := start + int64(limit)
	if end > int64(len(j.out.Output)) {
		end = int64(len(j.out.Output))
	}
	if remaining := total - offset; end > start+remaining {
		end = start + remaining
	}
	text := j.out.Output[int(start):int(end)]
	return spi.JobOutputPage{
		ID: id, Output: text, Offset: offset, NextCursor: offset + int64(len(text)), Total: total,
		Truncated: j.retainedOffset > 0, Done: j.out.Done, ExitCode: j.out.ExitCode, Cwd: j.out.Cwd,
	}, nil
}

func (j *integrationPager) calls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.pageCalls
}

type integrationSafeJob struct {
	*integrationPager
	stopErr   error
	stopCalls int
}

func (j *integrationSafeJob) Stop(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stopCalls++
	if j.stopErr != nil {
		return j.stopErr
	}
	if j.out.Done {
		return spi.ErrJobFinished
	}
	j.out.Done = true
	j.out.ExitCode = -1
	return nil
}

func (j *integrationSafeJob) Metadata(id string) (spi.JobMetadata, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return spi.JobMetadata{JobInfo: spi.JobInfo{ID: id, Done: j.out.Done}, RetainedOffset: j.retainedOffset, Total: j.producedTotal, Truncated: j.retainedOffset > 0}, nil
}

func (j *integrationSafeJob) controllerCalls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stopCalls
}

type jobActionWireResponse struct {
	job jobActionResponse
	raw map[string]any
}

type jobActionTestClient struct {
	requests chan string
	reader   *bufio.Reader
}

func (c *jobActionTestClient) request(t *testing.T, req jobActionRequest) jobActionWireResponse {
	t.Helper()
	if req.RequestID == "" {
		req.RequestID = "integration-request"
	}
	if req.SessionID == "" {
		req.SessionID = "session-integration"
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	c.requests <- jobsOutReqTag + string(raw)
	line, err := c.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取 serveJobsOut 应答: %v", err)
	}
	var resp jobActionResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, jobsOutRespTag))), &resp); err != nil {
		t.Fatalf("应答不是 jobActionResponse: %q: %v", line, err)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, jobsOutRespTag))), &generic); err != nil {
		t.Fatal(err)
	}
	return jobActionWireResponse{job: resp, raw: generic}
}

func startIntegrationJobsOut(t *testing.T, job spi.Job) *jobActionTestClient {
	t.Helper()
	previousRequests := jobsOutCh
	previousStderr := os.Stderr
	jobsOutCh = make(chan string, 16)
	readSide, writeSide, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writeSide
	k := kernel.New("jobs-integration-test")
	root := k.Root()
	if err := kernel.Provide(root, spi.JobKey, job); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		serveJobsOut(root, t.TempDir(), "session-integration")
		close(done)
	}()
	t.Cleanup(func() {
		close(jobsOutCh)
		<-done
		jobsOutCh = previousRequests
		os.Stderr = previousStderr
		_ = writeSide.Close()
		_ = readSide.Close()
		root.Dispose()
	})
	return &jobActionTestClient{requests: jobsOutCh, reader: bufio.NewReader(readSide)}
}

func TestServeJobsOutIntegrationPrefersPagerAndController(t *testing.T) {
	base := &integrationJob{out: spi.JobOutput{ID: "job-running", Output: "A甲🙂乙B", Done: false}}
	pager := &integrationPager{integrationJob: base, maxPageBytes: 2}
	job := &integrationSafeJob{integrationPager: pager}
	client := startIntegrationJobsOut(t, job)

	page := client.request(t, jobActionRequest{ID: "job-running", Cursor: 1, Limit: 2}).job
	if page.Error != "" || page.Output == nil || page.Output.Output != "甲🙂" || page.Output.Cursor != 1 || page.Output.NextCursor != 3 || page.Output.Total != 5 || !page.Output.HasMore {
		t.Errorf("rune 分页不正确: %+v error=%q", page.Output, page.Error)
	}
	if pager.calls() == 0 {
		t.Error("具备 spi.JobPager 时仍走了旧 Output，字节游标能力没有接入")
	}

	stopped := client.request(t, jobActionRequest{ID: "job-running", Limit: 1, Stop: true}).job
	if stopped.Error != "" || !stopped.Stopped || stopped.Output == nil || !stopped.Output.Done {
		t.Errorf("停止未确认 ended: %+v", stopped)
	}
	if job.controllerCalls() != 1 {
		t.Errorf("spi.JobController.Stop 调用次数=%d，want=1", job.controllerCalls())
	}
	if _, kills := base.counts(); kills != 0 {
		t.Errorf("具备 Controller 时旧 Kill 被调用 %d 次，want=0", kills)
	}

	ended := client.request(t, jobActionRequest{ID: "job-running", Cursor: 3, Limit: 2}).job
	if ended.Error != "" || ended.Output == nil || ended.Output.Output != "乙B" || !ended.Output.Done || ended.Output.HasMore {
		t.Errorf("停止后 readback 不正确: %+v error=%q", ended.Output, ended.Error)
	}
	second := client.request(t, jobActionRequest{ID: "job-running", Limit: 1, Stop: true}).job
	if second.Code != "job_finished" {
		t.Errorf("二次 stop code=%q，want=job_finished", second.Code)
	}
}

func TestServeJobsOutIntegrationPagerWaitPreservesLegacyEndedWait(t *testing.T) {
	base := &integrationJob{out: spi.JobOutput{ID: "job-wait", Output: "已有输出", Done: false}}
	pager := &integrationPager{integrationJob: base, maxPageBytes: 2}
	client := startIntegrationJobsOut(t, pager)
	timer := time.AfterFunc(40*time.Millisecond, func() {
		base.mu.Lock()
		base.out.Done = true
		base.mu.Unlock()
	})
	defer timer.Stop()
	started := time.Now()
	resp := client.request(t, jobActionRequest{ID: "job-wait", Limit: 1, WaitMs: 500}).job
	if resp.Error != "" || resp.Output == nil || !resp.Output.Done {
		t.Fatalf("Pager 有既有输出时没有等到 ended: %+v error=%q", resp.Output, resp.Error)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 450*time.Millisecond {
		t.Fatalf("waitMs 等待窗口不正确: %s", elapsed)
	}
}

func TestServeJobsOutIntegrationLegacyWithoutPagerOrController(t *testing.T) {
	job := &integrationJob{out: spi.JobOutput{ID: "job-legacy", Output: "甲🙂乙", Done: false}}
	client := startIntegrationJobsOut(t, job)
	page := client.request(t, jobActionRequest{ID: "job-legacy", Cursor: 1, Limit: 1}).job
	if page.Error != "" || page.Output == nil || page.Output.Output != "🙂" || page.Output.Total != 3 {
		t.Fatalf("旧 Output 兼容路径回归: %+v error=%q", page.Output, page.Error)
	}
	stopped := client.request(t, jobActionRequest{ID: "job-legacy", Limit: 1, Stop: true}).job
	if stopped.Error != "" || !stopped.Stopped {
		t.Fatalf("旧 Kill fallback 未确认停止: %+v", stopped)
	}
	outputs, kills := job.counts()
	if outputs == 0 || kills != 1 {
		t.Fatalf("旧能力调用次数 Output=%d Kill=%d，want Output>0 Kill=1", outputs, kills)
	}
}

func TestServeJobsOutIntegrationPagerTruncationAndEndedIdempotence(t *testing.T) {
	base := &integrationJob{out: spi.JobOutput{ID: "job-ended", Output: "甲🙂乙", Done: true}}
	pager := &integrationPager{integrationJob: base, retainedOffset: 7, maxPageBytes: 3}
	client := startIntegrationJobsOut(t, pager)
	first := client.request(t, jobActionRequest{ID: "job-ended", Cursor: 1, Limit: 1})
	second := client.request(t, jobActionRequest{ID: "job-ended", Cursor: 1, Limit: 1})
	if first.job.Error != "" || first.job.Output == nil || first.job.Output.Output != "🙂" || first.job.Output.Total != 3 || first.job.Output.NextCursor != 2 {
		t.Errorf("截断窗口的 rune 分页不正确: %+v error=%q", first.job.Output, first.job.Error)
	}
	if !reflect.DeepEqual(first.job.Output, second.job.Output) {
		t.Errorf("ended 同游标不幂等: first=%+v second=%+v", first.job.Output, second.job.Output)
	}
	if pager.calls() == 0 {
		t.Error("截断用例没有使用 Pager")
	}
	output, _ := first.raw["output"].(map[string]any)
	if output["truncated"] != true {
		t.Errorf("保留前缀已淘汰但 HTTP 未明确 truncated=true: %+v", first.raw)
	}
	if first.job.Output != nil && first.job.Output.Total == 10 {
		t.Error("HTTP Total 错用了 SPI 总字节数，rune/byte 契约被混淆")
	}
}

func TestServeJobsOutIntegrationRealW69RetentionWindow(t *testing.T) {
	svc := execbash.NewService(t.TempDir())
	t.Cleanup(svc.Close)
	helper := buildJobsIntegrationTreeHelper(t)
	command := fmt.Sprintf("%s output %s", quoteJobsIntegrationShell(filepath.ToSlash(helper)), quoteJobsIntegrationShell(filepath.ToSlash(t.TempDir())))
	started, err := svc.RunBackground(command)
	if err != nil {
		t.Fatal(err)
	}
	if started.JobID == "" {
		t.Fatalf("真实 W69 任务没有 id: %+v", started)
	}
	ended, err := svc.Output(started.JobID, 5*time.Second)
	if err != nil || !ended.Done {
		t.Fatalf("等待真实大输出任务结束: done=%v err=%v", ended.Done, err)
	}
	meta, err := svc.Metadata(started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	retainedBytes := meta.Total - meta.RetainedOffset
	if meta.Total != 1024*1024+10 || meta.RetainedOffset <= 0 || !meta.Truncated || retainedBytes != int64(len(ended.Output)) || retainedBytes <= 1024*1024-1024 || retainedBytes > 1024*1024 {
		t.Fatalf("真实 W69 保留窗口事实不成立: meta=%+v legacyBytes=%d", meta, len(ended.Output))
	}
	// 旧 Output 是同一保留文本的兼容投影；新 Pager 适配后的 HTTP rune Total
	// 必须与它一致，而不是等于 SPI 含淘汰前缀的总字节数。
	totalRunes := len([]rune(ended.Output))
	client := startIntegrationJobsOut(t, svc)
	first := client.request(t, jobActionRequest{ID: started.JobID, Cursor: 0, Limit: 1})
	if first.job.Error != "" || first.job.Output == nil || first.job.Output.Output != "x" || first.job.Output.Total != totalRunes || !first.job.Output.Done || !first.job.Output.HasMore {
		t.Fatalf("真实 1 MiB 保留窗口首页不正确: %+v error=%q", first.job.Output, first.job.Error)
	}
	output, _ := first.raw["output"].(map[string]any)
	if output["truncated"] != true {
		t.Fatalf("真实淘汰前缀未透出 truncated=true: %+v", first.raw)
	}
	tail := client.request(t, jobActionRequest{ID: started.JobID, Cursor: totalRunes - 3, Limit: 3})
	again := client.request(t, jobActionRequest{ID: started.JobID, Cursor: totalRunes - 3, Limit: 3})
	if tail.job.Error != "" || tail.job.Output == nil || tail.job.Output.Output != "甲🙂乙" || tail.job.Output.Total != totalRunes || tail.job.Output.NextCursor != totalRunes || tail.job.Output.HasMore {
		t.Fatalf("真实保留窗口尾部 rune 分页不正确: %+v error=%q", tail.job.Output, tail.job.Error)
	}
	if !reflect.DeepEqual(tail.job.Output, again.job.Output) {
		t.Fatalf("真实 ended 大输出同游标不幂等: first=%+v again=%+v", tail.job.Output, again.job.Output)
	}
}

func TestServeJobsOutIntegrationCrossInstanceKeepsCrossSessionCode(t *testing.T) {
	base := &integrationJob{out: spi.JobOutput{ID: "job-foreign", Output: "x"}, outputErr: spi.ErrJobWrongInstance}
	pager := &integrationPager{integrationJob: base}
	job := &integrationSafeJob{integrationPager: pager, stopErr: spi.ErrJobWrongInstance}
	client := startIntegrationJobsOut(t, job)
	for _, req := range []jobActionRequest{{ID: "job-foreign", Limit: 1}, {ID: "job-foreign", Limit: 1, Stop: true}} {
		resp := client.request(t, req).job
		if resp.Code != "cross_session" {
			t.Errorf("跨实例 Stop=%v code=%q，want=cross_session error=%q", req.Stop, resp.Code, resp.Error)
		}
	}
}

func TestServeJobsOutIntegrationPagerWindowIsBounded(t *testing.T) {
	base := &integrationJob{out: spi.JobOutput{ID: "job-huge", Output: "x", Done: true}}
	pager := &integrationPager{integrationJob: base, producedTotal: 1024*1024 + 1}
	client := startIntegrationJobsOut(t, pager)
	resp := client.request(t, jobActionRequest{ID: "job-huge", Limit: 1}).job
	if resp.Error == "" || !strings.Contains(resp.Error, "1 MiB") || resp.Output != nil {
		t.Errorf("超过适配上限的 Pager 窗口必须 fail-loud，不能无界读取或伪造页面: %+v", resp)
	}
	if pager.calls() == 0 {
		t.Error("有界检查必须发生在 Pager 元数据探测之后")
	}
}
