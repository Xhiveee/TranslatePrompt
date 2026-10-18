// Package translator translates Russian text to English using keyless
// endpoints: the unofficial Yandex Translate mobile API (srv=android),
// with automatic fallback to the free Google GTX endpoint. No API keys.
package translator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	yandexURL       = "https://translate.yandex.net/api/v1/tr.json/translate"
	yandexUserAgent = "ru.yandex.translate/21.15.4.21402814 (Xiaomi Redmi K20 Pro; Android 11)"
	googleGTXURL    = "https://translate.googleapis.com/translate_a/single"
	defaultTimeout  = 10 * time.Second
	maxTextLen      = 9000 // leave headroom under endpoint limits
)

// Client is a resilient keyless translator: Yandex first, Google GTX
// as fallback. Safe for concurrent use.
type Client struct {
	hc   *http.Client
	mu   sync.Mutex
	ucid string
	// endpoint overrides are only used by tests.
	yandexEndpoint string
	googleEndpoint string
}

func New() *Client {
	return &Client{
		hc:             &http.Client{Timeout: defaultTimeout},
		ucid:           newUCID(),
		yandexEndpoint: yandexURL,
		googleEndpoint: googleGTXURL,
	}
}

// Translate converts Russian text to English. If the text exceeds the
// endpoint size limit it is split on line/sentence boundaries and each
// chunk is translated and rejoined.
func (c *Client) Translate(text string) (string, error) {
	chunks := splitChunks(text, maxTextLen)
	out := make([]string, 0, len(chunks))
	for _, ch := range chunks {
		t, err := c.translateChunk(ch)
		if err != nil {
			return "", err
		}
		out = append(out, t)
	}
	return strings.Join(out, ""), nil
}

func (c *Client) translateChunk(text string) (string, error) {
	tr, err := c.yandex(text)
	if err == nil {
		return tr, nil
	}
	// On auth/rate-limit rejection rotate the ucid and try once more —
	// a fresh ucid usually restores access without any account.
	if isRetryable(err) {
		c.rotateUCID()
		if tr, err2 := c.yandex(text); err2 == nil {
			return tr, nil
		}
	}
	// Fallback to Google GTX.
	return c.googleGTX(text)
}

// --- Yandex unofficial mobile endpoint ---

type yandexResponse struct {
	Code int      `json:"code"`
	Lang string   `json:"lang"`
	Text []string `json:"text"`
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func isRetryable(err error) bool {
	if ae, ok := err.(*apiError); ok {
		return ae.code == http.StatusForbidden || ae.code == http.StatusTooManyRequests
	}
	return false
}

func (c *Client) yandex(text string) (string, error) {
	c.mu.Lock()
	ucid := c.ucid
	endpoint := c.yandexEndpoint
	c.mu.Unlock()

	u := fmt.Sprintf("%s?ucid=%s&srv=android&format=text", endpoint, ucid)
	form := url.Values{"text": {text}, "lang": {"ru-en"}}

	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", yandexUserAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("yandex request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("yandex read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &apiError{code: resp.StatusCode,
			msg: fmt.Sprintf("yandex http %d: %.200s", resp.StatusCode, body)}
	}

	var yr yandexResponse
	if err := json.Unmarshal(body, &yr); err != nil {
		return "", fmt.Errorf("yandex decode: %w", err)
	}
	if yr.Code != 200 {
		return "", &apiError{code: yr.Code,
			msg: fmt.Sprintf("yandex api code %d", yr.Code)}
	}
	if len(yr.Text) == 0 {
		return "", fmt.Errorf("yandex empty result")
	}
	return strings.Join(yr.Text, ""), nil
}

// --- Google GTX fallback endpoint ---
// Response shape: [[["translated","source",...],["more","...",...]],...]

func (c *Client) googleGTX(text string) (string, error) {
	u := fmt.Sprintf("%s?client=gtx&sl=ru&tl=en&dt=t&q=%s",
		c.googleEndpoint, url.QueryEscape(text))

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", yandexUserAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("google gtx request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("google gtx read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &apiError{code: resp.StatusCode,
			msg: fmt.Sprintf("google gtx http %d: %.200s", resp.StatusCode, body)}
	}

	var payload []any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("google gtx decode: %w", err)
	}
	if len(payload) == 0 {
		return "", fmt.Errorf("google gtx empty result")
	}
	segments, ok := payload[0].([]any)
	if !ok {
		return "", fmt.Errorf("google gtx malformed payload")
	}
	var sb strings.Builder
	for _, seg := range segments {
		parts, ok := seg.([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		if s, ok := parts[0].(string); ok {
			sb.WriteString(s)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("google gtx produced no text")
	}
	return sb.String(), nil
}

func (c *Client) rotateUCID() {
	c.mu.Lock()
	c.ucid = newUCID()
	c.mu.Unlock()
}

// newUCID returns a UUIDv4 as 32 lowercase hex chars (no hyphens),
// matching what the Yandex Android client sends.
func newUCID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially unreachable; fall back to
		// time-seeded bytes rather than panic inside a daemon.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (i % 8 * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return hex.EncodeToString(b[:])
}

// splitChunks breaks text at newline/sentence boundaries so each piece
// stays under limit without cutting words.
func splitChunks(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > limit {
		cut := limit
		if idx := strings.LastIndex(text[:limit], "\n"); idx > limit/2 {
			cut = idx + 1
		} else if idx := strings.LastIndexAny(text[:limit], ".!? "); idx > limit/2 {
			cut = idx + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
