// channel.go 模型名的「渠道前缀」：把调用渠道写进模型名，网关据此选线。
//
// 约定（可被 config.model_prefixes 覆盖/扩展）。前缀名与 realm 名**同名**，好记也好排错：
//
//	workbuddy/<模型>  → workbuddy（WorkBuddy AI 线 / www.workbuddy.ai）
//	codebuddy/<模型>  → codebuddy（CodeBuddy IDE 域 / www.codebuddy.ai，workbuddy 的路线别名）
//
// 设计要点：
//   - 前缀只是**路由与标识**：出站前由调用方剥离，上游只看到裸模型名；
//   - 前缀大小写不敏感；
//   - **未知前缀不做剥离**（整串按模型名透传），避免误伤本身含 "/" 的模型 id；
//   - 剥离后裸模型名为空（形如 "codebuddy/"）时按"无前缀"处理，绝不把空模型名发给上游。
package realm

import "strings"

// ChannelPrefixSeparator 渠道前缀与模型名的分隔符。
const ChannelPrefixSeparator = "/"

// DefaultChannelPrefixes 内置渠道前缀映射（config.model_prefixes 会在此之上覆盖/扩展）。
func DefaultChannelPrefixes() map[string]string {
	return map[string]string{
		WB: WB,
		CB: CB,
	}
}

// NormalizeChannelPrefixes 归一前缀映射：内置默认 + 调用方覆盖。
// 非法条目（空前缀、含分隔符/空白、未知 realm）一律**丢弃**；配置层已先行校验并报错，
// 这里只兜底，保证运行时拿到的永远是一张可用的表。
func NormalizeChannelPrefixes(in map[string]string) map[string]string {
	out := DefaultChannelPrefixes()
	for k, v := range in {
		p := strings.ToLower(strings.TrimSpace(k))
		if p == "" || strings.ContainsAny(p, "/:\\ \t") {
			continue
		}
		rn := strings.TrimSpace(v)
		if !Known(rn) {
			continue
		}
		out[p] = Normalize(rn)
	}
	return out
}

// SplitChannelPrefix 拆分模型名的渠道前缀。
//
// 命中已知前缀时返回 (前缀, 裸模型名, 该前缀对应的 realm)；
// 未命中（没有分隔符 / 前缀未知 / 裸名为空）时返回 ("", model, "")，调用方原样透传。
func SplitChannelPrefix(model string, prefixes map[string]string) (prefix, bare, rn string) {
	trimmed := strings.TrimSpace(model)
	i := strings.Index(trimmed, ChannelPrefixSeparator)
	if i <= 0 || i >= len(trimmed)-1 {
		return "", model, ""
	}
	p := strings.ToLower(strings.TrimSpace(trimmed[:i]))
	target, ok := prefixes[p]
	if !ok {
		return "", model, ""
	}
	bare = strings.TrimSpace(trimmed[i+1:])
	if bare == "" {
		return "", model, ""
	}
	return p, bare, Normalize(target)
}

// ChannelPrefixFor 返回该 realm 的主渠道前缀（无映射时返回 ""）。
// /v1/models 用它把模型名呈现成带渠道前缀的形式，让客户端能显式选渠道。
// 同一 realm 有多条前缀时取字典序最小的一条，保证输出稳定。
func ChannelPrefixFor(rn string, prefixes map[string]string) string {
	want := Normalize(rn)
	best := ""
	for p, target := range prefixes {
		if Normalize(target) != want {
			continue
		}
		if best == "" || p < best {
			best = p
		}
	}
	return best
}
