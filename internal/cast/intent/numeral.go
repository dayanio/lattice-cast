// 本文件：基础中文数字解析（阿拉伯数字与「一二两三四五六七八九十」及其
// 组合，如「十五」「二十五」）。只覆盖快通道句式里真实出现的量（分钟数/
// 音量值），更大或更复杂的数交回 LLM。
package intent

import (
	"strconv"
	"strings"
)

// zhDigit 是单个中文数字位（「两」作「二」的口语形一并收录）。
var zhDigit = map[rune]int{
	'一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
}

// parseCount 解析非负整数：阿拉伯数字原样收，中文数字按「十位组合」解析
// （十→10、十五→15、二十→20、二十五→25，上限九十九）。ok=false 表示不是
// 可识别的数，由调用方决定是否落回 LLM。
func parseCount(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	total, cur := 0, 0
	for _, ch := range s {
		if ch == '十' {
			if cur == 0 {
				cur = 1 // 「十」「十五」的十位缺省按一
			}
			total += cur * 10
			cur = 0
			continue
		}
		d, ok := zhDigit[ch]
		if !ok {
			return 0, false // 百/千/负号/其他字符：超出基础范围，如实报不可解析
		}
		cur = d
	}
	return total + cur, true
}
