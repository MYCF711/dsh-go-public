package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 这条钉的是**段内布局顺序**：目录树必须在最前，数据正文在后面。
//
// 为什么值得一条测试：反过来的写法（数据在前）**编译、链接、运行全都正常**，
// 只有 Windows 读 exe 的 VersionInfo 时才表现为"全空"。也就是说这个错误不会
// 在任何一条普通测试里露头——它会安安静静地让任务管理器的"描述"列变回空白，
// 而那正是这一整项功能存在的理由。
//
// 实测踩过：早先版本就是数据在前，PowerShell 读出来 FileDescription/ProductName/
// CompanyName 全是空字符串，而 `go build` 一声不响。
func TestResourceSectionPutsDirectoryTreeFirst(t *testing.T) {
	blob := []byte("FAKE-VERSION-BLOB-0123456789")
	sec, dataEntryOff := resourceSection(blob)

	if len(sec) < 16 {
		t.Fatalf("段太短：%d 字节", len(sec))
	}

	// 段首必须是"根目录"：16 字节头 + 1 条项，命名 0 个、ID 1 个。
	named := binary.LittleEndian.Uint16(sec[12:14])
	ids := binary.LittleEndian.Uint16(sec[14:16])
	if named != 0 || ids != 1 {
		t.Fatalf("段首不是根目录头（named=%d ids=%d）——目录树被数据挤到后面了", named, ids)
	}
	if len(sec) >= 4 && binary.LittleEndian.Uint32(sec[:4]) == binary.LittleEndian.Uint32(blob[:4]) {
		t.Fatalf("段首就是版本正文——数据排在目录树前面了")
	}

	// 数据项里的 OffsetToData 必须是**段内偏移**（重定位前），且指向 blob 起点。
	rva := binary.LittleEndian.Uint32(sec[dataEntryOff : dataEntryOff+4])
	if int(rva) < dataEntryOff+16 || int(rva) > len(sec) {
		t.Fatalf("数据项 OffsetToData=%d 不像段内偏移（数据项在 %d，段长 %d）", rva, dataEntryOff, len(sec))
	}
	if !bytes.HasPrefix(sec[rva:], blob) {
		t.Fatalf("OffsetToData 指向的内容不是版本正文：%q", sec[rva:min(len(sec), int(rva)+16)])
	}

	// 声明的大小必须与真实 blob 一致，否则资源管理器按大小读会截断/越界。
	if got := binary.LittleEndian.Uint32(sec[dataEntryOff+4 : dataEntryOff+8]); int(got) != len(blob) {
		t.Fatalf("数据项声明大小 %d ≠ blob 长度 %d", got, len(blob))
	}
}

// 段必须是 4 字节对齐的：PE 的 .rsrc 段按 4 对齐，不对齐时资源管理器行为未定义。
func TestResourceSectionAligned(t *testing.T) {
	for _, n := range []int{1, 3, 4, 5, 17} {
		sec, _ := resourceSection(make([]byte, n))
		if len(sec)%4 != 0 {
			t.Fatalf("blob=%d 时段长 %d 不是 4 的倍数", n, len(sec))
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
