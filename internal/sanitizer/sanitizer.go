// Package sanitizer prepares Russian developer prompts for keyless
// translation endpoints: it masks technical tokens (code, paths, flags,
// @mentions) behind [[N]] placeholders and pre-replaces Russian dev slang
// with English words, then restores everything after translation.
package sanitizer

import (
	"regexp"
	"sort"
)

// maskPatterns are applied in order; earlier rules win on overlaps.
// Placeholders are [[0]], [[1]], ... — digits survive translation
// because the free endpoints leave numeric tokens alone.
var maskPatterns = []*regexp.Regexp{
	// Fenced code blocks: ```...```
	regexp.MustCompile("(?s)```.*?```"),
	// Inline code: `...`
	regexp.MustCompile("`[^`\\n]+`"),
	// Agent mentions / slash commands: @file.go, @dir/, /commit
	regexp.MustCompile(`(?:^|\s)(@[a-zA-Z0-9_./:-]+|/[a-zA-Z0-9_-]+)`),
	// Windows and Unix paths: C:\x\y, /usr/bin, ./rel/path, ../rel
	regexp.MustCompile(`(?:[a-zA-Z]:\\[^\s"'<>|]+|(?:\.{1,2}/|/)[a-zA-Z0-9_./-]+)`),
	// CLI flags: -v, --force, --out=path
	regexp.MustCompile(`(?:^|\s)(--?[a-zA-Z0-9_-]+(?:=\S+)?)`),
}

// Translators occasionally mangle placeholders ([[ 3]], «[[3]]» etc.),
// so unmasking tolerates spaces inside the brackets.
var placeholderRe = regexp.MustCompile(`\[\[\s*(\d+)\s*\]\]`)

// Mask replaces technical tokens with [[N]] placeholders.
// The returned restore closure maps placeholders back to originals.
func Mask(text string) (masked string, originals []string) {
	masked = text
	for _, re := range maskPatterns {
		masked = re.ReplaceAllStringFunc(masked, func(m string) string {
			originals = append(originals, m)
			return placeholder(len(originals) - 1)
		})
	}
	return masked, originals
}

func placeholder(i int) string {
	return "[[" + itoa(i) + "]]"
}

// Unmask restores [[N]] placeholders to their original tokens.
func Unmask(text string, originals []string) string {
	return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		n, ok := parsePlaceholder(m)
		if !ok || n < 0 || n >= len(originals) {
			return m
		}
		return originals[n]
	})
}

func parsePlaceholder(m string) (int, bool) {
	inner := placeholderRe.FindStringSubmatch(m)
	if len(inner) != 2 {
		return 0, false
	}
	n := 0
	for _, c := range inner[1] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// ApplySlang pre-replaces Russian dev slang with English words using
// Unicode-aware word boundaries (Go's \b is ASCII-only and would not
// bound Cyrillic words). Matching is case-insensitive.
func ApplySlang(text string, dictionary map[string]string) string {
	if len(dictionary) == 0 || text == "" {
		return text
	}
	// Longer keys first so "замокать" wins over any shorter prefix that
	// might also be in the map.
	keys := make([]string, 0, len(dictionary))
	for k := range dictionary {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	out := text
	for _, k := range keys {
		re := regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}_])` + regexp.QuoteMeta(k) + `([^\p{L}\p{N}_]|$)`)
		out = re.ReplaceAllString(out, "${1}"+dictionary[k]+"${2}")
	}
	return out
}

// Process runs the full pre-translation pipeline and returns the masked,
// slang-normalized text plus the restore closure for the result.
func Process(text string, slang map[string]string) (prepared string, originals []string) {
	masked, originals := Mask(text)
	return ApplySlang(masked, slang), originals
}

// Restore is Unmask as a pipeline counterpart of Process.
func Restore(text string, originals []string) string {
	return Unmask(text, originals)
}

// itoa avoids pulling strconv into call sites; kept tiny.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
