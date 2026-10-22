// Package safename 校验并清洗会被拼进文件路径的外部标识符（会话 ID、
// 记忆名、pipeline 节点名、tool_use ID 等），防止路径穿越。
//
// 规则对 Unicode 友好（中文名合法），只拒绝在 Windows/Unix 上会改变
// 路径语义或无法作为文件名的写法。
package safename

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLen 名称允许的最大字符数（按 rune 计）。
const MaxLen = 128

// reservedChars 在 Windows 文件名中非法或会改变路径语义的字符。
const reservedChars = `/\:*?"<>|`

// windowsReserved Windows 保留的设备名（不区分大小写，忽略扩展名）。
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Check 校验 s 能否安全地作为单个文件名使用；kind 用于错误信息（如 "会话 ID"）。
func Check(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s 不能为空", kind)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s 不是合法的 UTF-8", kind)
	}
	if utf8.RuneCountInString(s) > MaxLen {
		return fmt.Errorf("%s 超过 %d 个字符", kind, MaxLen)
	}
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return fmt.Errorf("%s %q 含有 \"..\"", kind, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return fmt.Errorf("%s %q 含有控制字符", kind, s)
		}
		if strings.ContainsRune(reservedChars, r) {
			return fmt.Errorf("%s %q 含有非法字符 %q", kind, s, r)
		}
	}
	if s != strings.TrimSpace(s) || strings.HasSuffix(s, ".") {
		return fmt.Errorf("%s %q 不能以空白开头或结尾、不能以点结尾", kind, s)
	}
	base := s
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return fmt.Errorf("%s %q 是 Windows 保留设备名", kind, s)
	}
	return nil
}

// Sanitize 把任意字符串清洗成可用的文件名：非法字符替换为 '_'，去掉首尾
// 空白和结尾的点，截断到 MaxLen。结果为空或仍不合法时返回 "_"。
func Sanitize(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= MaxLen {
			break
		}
		if r == utf8.RuneError || r < 0x20 || r == 0x7f || unicode.IsControl(r) || strings.ContainsRune(reservedChars, r) {
			r = '_'
		}
		b.WriteRune(r)
		n++
	}
	out := strings.TrimRight(strings.TrimSpace(b.String()), ".")
	out = strings.ReplaceAll(out, "..", "__")
	if Check("name", out) != nil {
		return "_"
	}
	return out
}

// Join 校验 name 后拼接 dir/name+ext，并确认结果仍位于 dir 之内。
func Join(kind, dir, name, ext string) (string, error) {
	if err := Check(kind, name); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name+ext)
	rel, err := filepath.Rel(filepath.Clean(dir), p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s %q 越出目录 %s", kind, name, dir)
	}
	return p, nil
}
