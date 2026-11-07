package schedule

import (
	"encoding/json"
	"fmt"
)

const (
	// currentScheduleVersion 是所有 schedule 写路径唯一允许落盘的版本。
	currentScheduleVersion = 1
	// CurrentScheduleVersion 给登记工具复用，避免两层各自维护一个会漂移的当前值。
	CurrentScheduleVersion = currentScheduleVersion
)

// compatibleScheduleVersions 是显式读取白名单；无 version 的 legacy 不伪装成 version=0。
// 未来新增版本时，只有真正实现并测试了该版本的读取规则，才能把它加进表里。
var compatibleScheduleVersions = map[int]struct{}{
	currentScheduleVersion: {},
}

// ValidateScheduleVersion 校验整单元 JSON 的版本字段。
//
// 兼容规则刻意很小：字段缺失表示历史 legacy 形状，直接按当前 schedules 字段读取；
// 字段一旦存在，就必须是白名单里的整数。null、字符串、0 和未来版本都不能冒充 legacy。
// 本函数不改变 allow_writes 等条目字段，历史缺字段仍由 Go 零值保证 false。
func ValidateScheduleVersion(data []byte) error {
	var header struct {
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	if header.Version == nil {
		return nil
	}

	// 指针用来区分数字 0 与 null：两者都不是“字段缺失”，也都不可读。
	var version *int
	if err := json.Unmarshal(header.Version, &version); err != nil {
		return fmt.Errorf("定时任务文件 version 必须是兼容版本整数: %w", err)
	}
	if version == nil {
		return fmt.Errorf("定时任务文件 version 不能是 null；只有字段缺失才按 legacy 读取")
	}
	if _, ok := compatibleScheduleVersions[*version]; !ok {
		return fmt.Errorf("不支持的定时任务文件 version=%d：不在兼容版本白名单", *version)
	}
	return nil
}
