package sanitizer

import (
	"strings"
	"testing"
)

func TestMaskUnmaskRoundTrip(t *testing.T) {
	src := "запусти `go build ./...` в C:\\proj\\app и открой @file.go с --verbose\n```go\nfmt.Println(1)\n```"
	masked, originals := Mask(src)
	if strings.Contains(masked, "go build") {
		t.Fatalf("inline code leaked into masked text: %q", masked)
	}
	if strings.Contains(masked, "C:\\proj") {
		t.Fatalf("path leaked into masked text: %q", masked)
	}
	got := Unmask(masked, originals)
	if got != src {
		t.Fatalf("round trip mismatch:\n got: %q\nwant: %q", got, src)
	}
}

func TestMaskToleratesSpacedPlaceholder(t *testing.T) {
	masked, originals := Mask("run `cmd` now")
	// Simulate translator mangling the placeholder.
	mangled := strings.ReplaceAll(masked, "[[0]]", "[[ 0 ]]")
	got := Unmask(mangled, originals)
	if !strings.Contains(got, "`cmd`") {
		t.Fatalf("spaced placeholder not restored: %q", got)
	}
}

func TestSlangReplacesWordsCaseInsensitive(t *testing.T) {
	dict := map[string]string{"ручка": "endpoint", "фронт": "frontend"}
	got := ApplySlang("проверь РУЧКА и фронт", dict)
	if !strings.Contains(got, "endpoint") || !strings.Contains(got, "frontend") {
		t.Fatalf("slang not applied: %q", got)
	}
}

func TestSlangDoesNotReplaceSubstrings(t *testing.T) {
	dict := map[string]string{"фронт": "frontend"}
	got := ApplySlang("фронтальный удар", dict)
	if strings.Contains(got, "frontend") {
		t.Fatalf("substring replaced: %q", got)
	}
}

func TestLiteralPlaceholdersSurvive(t *testing.T) {
	src := "объясни `cmd` и [[0]] и [[ 7 ]]"
	masked, originals := Mask(src)
	got := Unmask(masked, originals)
	if got != src {
		t.Fatalf("literal placeholders not preserved:\n got: %q\nwant: %q", got, src)
	}
	// The literal [[0]] must not be duplicated by a generated token.
	if strings.Count(got, "`cmd`") != 1 {
		t.Fatalf("masked token leaked into literal text: %q", got)
	}
}

func TestSlangAdjacentOccurrences(t *testing.T) {
	dict := map[string]string{"ручка": "endpoint"}
	got := ApplySlang("ручка ручка ручка", dict)
	if got != "endpoint endpoint endpoint" {
		t.Fatalf("adjacent slang not replaced: %q", got)
	}
}

func TestProcessPipeline(t *testing.T) {
	dict := map[string]string{"сбилди": "build"}
	src := "сбилди ./cmd/app"
	prepared, originals := Process(src, dict)
	if !strings.Contains(prepared, "build") {
		t.Fatalf("slang missing: %q", prepared)
	}
	if strings.Contains(prepared, "./cmd/app") {
		t.Fatalf("path not masked: %q", prepared)
	}
	// Masks are restored, but slang replacements intentionally persist
	// as English words in the output.
	got := Restore(prepared, originals)
	if got != "build ./cmd/app" {
		t.Fatalf("restore mismatch: %q", got)
	}
}
