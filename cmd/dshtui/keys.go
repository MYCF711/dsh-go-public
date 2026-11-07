package main

// CSI/SS3 按键名映射。修饰键必须明确识别：Shift+左右等尚未实现的
// 选择动作不能假借成普通移动；F5/F6/F7/F12 在上游只有键名，没有动作。

// 让既有 --script 的 key 步复用同一组名字。script.go 的表保留其原有
// 滚动键，这里只增量登记编辑器与解码器真实支持的键，不复制分发逻辑。
func init() {
	for _, name := range []string{
		"left", "right", "ctrl+left", "ctrl+right", "alt+left", "alt+right",
		"ctrl+home", "ctrl+end", "delete", "f5", "f6", "f7", "f12",
	} {
		specialKeys[name] = name
	}
	keyRunes["backspace"] = 127
}

// keyName 把 CSI/SS3 的参数+终结字节认成按键名。
//
// 只认确定知道的序列，认不出来返回空串（静默吞掉）：往输入行里塞
// "[1;2C" 比没反应糟得多。Ctrl+Home/End 保留正文滚动；plain Home/End
// 则由输入编辑器接管。
func keyName(seq []byte) string {
	if len(seq) == 0 {
		return ""
	}
	// SGR 鼠标：ESC [ <button;x;y M/m。保留参数给 app 做立绘命中测试，
	// 解码层只保证整段不会漏成输入字符。
	if seq[0] == '<' && (seq[len(seq)-1] == 'M' || seq[len(seq)-1] == 'm') {
		return "mouse:" + string(seq)
	}
	// 老 Linux 控制台 F5：上游表记为 `[[E`（含 CSI 的 '['）；本解码器
	// 把序列前缀放在 csi 布尔里，所以此处实际收到 `[E`。兼容 SS3 变体。
	if raw := string(seq); raw == "[E" || raw == "[[E" {
		return "f5"
	}

	numPart := seq[:len(seq)-1]
	modifier := ""
	if i := indexByte(numPart, ';'); i >= 0 {
		modifier = string(numPart[i+1:])
		numPart = numPart[:i]
	}
	num, _ := atoiSmall(string(numPart)) // SS3 的 "OA" 没有数字，得到 0
	last := seq[len(seq)-1]

	wordModifier := func(prefix string) string {
		switch modifier {
		case "5":
			return "ctrl+" + prefix
		case "3":
			return "alt+" + prefix
		default:
			return ""
		}
	}
	lineModifier := func(prefix string) string {
		if modifier == "5" {
			return "ctrl+" + prefix
		}
		if modifier == "" {
			return prefix
		}
		return ""
	}

	switch last {
	case 'A':
		if modifier == "" {
			return "up"
		}
	case 'B':
		if modifier == "" {
			return "down"
		}
	case 'C':
		if modifier == "" {
			return "right"
		}
		return wordModifier("right")
	case 'D':
		if modifier == "" {
			return "left"
		}
		return wordModifier("left")
	case 'H':
		return lineModifier("home")
	case 'F':
		return lineModifier("end")
	case '~':
		// F 键没有本端动作，但名字必须如实到达 handleSpecial；未绑定
		// 动作的默认分支不会改草稿、滚动或发送状态。
		if modifier == "" {
			switch num {
			case 3:
				return "delete"
			case 5:
				return "pgup"
			case 6:
				return "pgdn"
			case 15:
				return "f5"
			case 17:
				return "f6"
			case 18:
				return "f7"
			case 24:
				return "f12"
			}
		}
		switch num {
		case 1, 7:
			return lineModifier("home") // ESC[1~ / ESC[7~
		case 4, 8:
			return lineModifier("end") // ESC[4~ / ESC[8~
		}
	}
	return ""
}

// indexByte 是 bytes.IndexByte 的最小替代：这里只处理几个字节，不值得再引一个包。
func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
