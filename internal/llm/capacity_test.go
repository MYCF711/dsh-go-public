package llm

import "testing"

func TestResolveModelCapacity(t *testing.T) {
	const known = "group/deepseek-v4.1-flash-1017"
	tests := []struct {
		name     string
		modelID  string
		declared ModelCapacity
		want     ModelCapacity
	}{
		{
			name:    "未声明自定义模型使用十进制1M和128K",
			modelID: "custom/undeclared",
			want:    ModelCapacity{ContextWindow: 1000000, MaxOutput: 128000},
		},
		{
			name:     "显式32K和8K覆盖不变",
			modelID:  "custom/small",
			declared: ModelCapacity{ContextWindow: 32000, MaxOutput: 8000},
			want:     ModelCapacity{ContextWindow: 32000, MaxOutput: 8000},
		},
		{
			name:     "更小端点覆盖优先",
			modelID:  "custom/tiny",
			declared: ModelCapacity{ContextWindow: 16000, MaxOutput: 4000},
			want:     ModelCapacity{ContextWindow: 16000, MaxOutput: 4000},
		},
		{
			name:     "自定义模型逐字段声明",
			modelID:  "custom/context-only",
			declared: ModelCapacity{ContextWindow: 32000},
			want:     ModelCapacity{ContextWindow: 32000, MaxOutput: 128000},
		},
		{
			name:    "内置已知模型保持原容量",
			modelID: known,
			want:    ModelCapacity{ContextWindow: 128000, MaxOutput: 0},
		},
		{
			name:     "已知模型也尊重显式覆盖",
			modelID:  known,
			declared: ModelCapacity{ContextWindow: 32000, MaxOutput: 8000},
			want:     ModelCapacity{ContextWindow: 32000, MaxOutput: 8000},
		},
		{
			name:    "空模型ID不伪造未知容量",
			modelID: "",
			want:    ModelCapacity{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveModelCapacity(tc.modelID, tc.declared); got != tc.want {
				t.Fatalf("ResolveModelCapacity(%q, %+v) = %+v，期望 %+v", tc.modelID, tc.declared, got, tc.want)
			}
		})
	}
}

func TestResolveModelCapacityDoesNotLeakAcrossModels(t *testing.T) {
	const known = "group/deepseek-v4.1-flash-1017"
	sequence := []struct {
		modelID  string
		declared ModelCapacity
		want     ModelCapacity
	}{
		{modelID: "custom/a", want: ModelCapacity{ContextWindow: 1000000, MaxOutput: 128000}},
		{modelID: "custom/b", declared: ModelCapacity{ContextWindow: 32000, MaxOutput: 8000}, want: ModelCapacity{ContextWindow: 32000, MaxOutput: 8000}},
		{modelID: known, want: ModelCapacity{ContextWindow: 128000, MaxOutput: 0}},
		{modelID: "custom/a", want: ModelCapacity{ContextWindow: 1000000, MaxOutput: 128000}},
	}
	for i, step := range sequence {
		if got := ResolveModelCapacity(step.modelID, step.declared); got != step.want {
			t.Fatalf("第 %d 次解析（model=%q）= %+v，期望 %+v；发生了跨模型串值", i+1, step.modelID, got, step.want)
		}
	}
}
