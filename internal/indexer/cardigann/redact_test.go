package cardigann

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRedactURL(t *testing.T) {
	cases := map[string][]string{
		"https://t.example/api?t=search&apikey=SECRET1&q=dune":            {"SECRET1"},
		"https://t.example/dl.php?id=5&passkey=SECRET2":                   {"SECRET2"},
		"https://u:SECRET3@t.example/x":                                   {"SECRET3"},
		"https://t.example/rss/download/123/0123456789abcdef0123456789ab": {"0123456789abcdef0123456789ab"},
		"https://t.example/x?Cookie=SECRET4&API_KEY=SECRET5":              {"SECRET4", "SECRET5"},
		"https://t.example/x?q=%zz&passkey=SECRET6":                       {"SECRET6"},
	}
	for in, secrets := range cases {
		out := RedactURL(in)
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s -> %s still has %s", in, out, s)
			}
		}
		if !strings.Contains(out, "t.example") {
			t.Errorf("%s -> %s lost the host", in, out)
		}
	}
	if got := RedactURL("https://t.example/search?q=dune"); !strings.Contains(got, "q=dune") {
		t.Errorf("harmless query changed: %s", got)
	}
	msg := `Get "https://t.example/api?apikey=SECRET7": dial tcp: i/o timeout`
	if got := RedactText(msg); strings.Contains(got, "SECRET7") || !strings.Contains(got, "i/o timeout") {
		t.Errorf("text: %s", got)
	}
}

// A transport error carries the request URL; it must not leak the key.
func TestNetErrorRedactsURL(t *testing.T) {
	c := &http.Client{Timeout: 50 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://127.0.0.1:1/api?apikey=SECRET8&passkey=SECRET9", nil)
	_, err := c.Do(req)
	if err == nil {
		t.Skip("port 1 answered")
	}
	err = netError(err)
	if s := err.Error(); strings.Contains(s, "SECRET8") || strings.Contains(s, "SECRET9") {
		t.Fatalf("leak: %s", s)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("classification lost: %v", err)
	}
}
