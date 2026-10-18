package translator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestYandexPrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.RawQuery, "srv=android") {
			t.Errorf("missing srv=android in %s", r.URL.RawQuery)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("lang") != "ru-en" {
			t.Errorf("lang=%q", r.PostForm.Get("lang"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "lang": "ru-en", "text": []string{"hello world"},
		})
	}))
	defer srv.Close()

	c := New()
	c.yandexEndpoint = srv.URL
	got, err := c.Translate("привет мир")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestFallbackToGoogleGTX(t *testing.T) {
	ySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusInternalServerError)
	}))
	defer ySrv.Close()

	gSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GTX shape: [[["translated","source",null,null,3]],...]
		w.Write([]byte(`[[["build the frontend","сбилди фронт",null,null,3]],null,"ru"]`))
	}))
	defer gSrv.Close()

	c := New()
	c.yandexEndpoint = ySrv.URL
	c.googleEndpoint = gSrv.URL
	got, err := c.Translate("сбилди фронт")
	if err != nil {
		t.Fatal(err)
	}
	if got != "build the frontend" {
		t.Fatalf("got %q", got)
	}
}

func TestUCIDRollsOn429(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "text": []string{"ok"},
		})
	}))
	defer srv.Close()

	c := New()
	c.yandexEndpoint = srv.URL
	firstUCID := c.ucid
	got, err := c.Translate("ок")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" {
		t.Fatalf("got %q", got)
	}
	if c.ucid == firstUCID {
		t.Fatal("ucid was not rotated after 429")
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestSplitChunks(t *testing.T) {
	long := strings.Repeat("слово. ", 4000) // ~28KB
	chunks := splitChunks(long, maxTextLen)
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != long {
		t.Fatal("chunks do not reassemble to original")
	}
	for _, ch := range chunks {
		if len(ch) > maxTextLen {
			t.Fatalf("chunk too large: %d", len(ch))
		}
	}
}

func TestSplitChunksKeepsUTF8Intact(t *testing.T) {
	// Unbroken Cyrillic text — no delimiters, so cuts fall wherever
	// the byte limit lands.
	long := strings.Repeat("ё", maxTextLen*2) // 2 bytes per rune
	chunks := splitChunks(long, maxTextLen)
	if strings.Join(chunks, "") != long {
		t.Fatal("chunks do not reassemble to original")
	}
	for _, ch := range chunks {
		if !utf8.ValidString(ch) {
			t.Fatalf("chunk splits a rune: %q...", ch[:20])
		}
	}
}
