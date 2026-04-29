package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebSearchToolParsesResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "golang agent tools" {
			t.Fatalf("unexpected query: %q", got)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`
			<div class="result">
				<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fone">First <b>Result</b></a>
				<a class="result__snippet">A &amp; B snippet.</a>
			</div>
			<div class="result">
				<a class="result__a" href="https://example.com/two">Second Result</a>
				<div class="result__snippet">Another snippet.</div>
			</div>
		`))
	}))
	defer server.Close()

	result, err := NewWebSearchToolWithClient(server.Client(), server.URL).Call(map[string]any{
		"query": "golang agent tools",
		"limit": 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded []webSearchResult
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, result)
	}
	if len(decoded) != 1 {
		t.Fatalf("expected 1 result, got %d", len(decoded))
	}
	if decoded[0].Title != "First Result" {
		t.Fatalf("unexpected title: %q", decoded[0].Title)
	}
	if decoded[0].URL != "https://example.com/one" {
		t.Fatalf("unexpected url: %q", decoded[0].URL)
	}
	if decoded[0].Snippet != "A & B snippet." {
		t.Fatalf("unexpected snippet: %q", decoded[0].Snippet)
	}
}

func TestWebSearchToolUsesContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewWebSearchToolWithClient(server.Client(), server.URL).CallContext(ctx, map[string]any{
		"query": "cancelled",
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestDefaultRegistryIncludesWebSearch(t *testing.T) {
	registry := NewDefaultRegistry()
	if _, err := registry.Get("websearch"); err != nil {
		t.Fatalf("default registry missing websearch: %v", err)
	}
}
