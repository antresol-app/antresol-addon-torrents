package coreadapt_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol/addonkit"
)

func TestImages_Fetch(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/addon-core/v1/images" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"name":"0123456789abcdef0123456789abcdef01234567.jpg"}`))
	}))
	defer srv.Close()
	name, err := coreadapt.Images{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL})}.Fetch(ctx, "https://image.tmdb.org/t/p/w342/x.jpg")
	if err != nil || name != "0123456789abcdef0123456789abcdef01234567.jpg" || got["url"] != "https://image.tmdb.org/t/p/w342/x.jpg" {
		t.Fatalf("%q %v %v", name, err, got)
	}
}

// The core's recorded answer: the cached image's name ({image} in the golden).
func TestImages_FetchReplayed(t *testing.T) {
	if name, err := (coreadapt.Images{C: replay(t)}).Fetch(ctx, "https://image.tmdb.org/t/p/w342/x.jpg"); err != nil || name != "{image}" {
		t.Fatalf("%q %v", name, err)
	}
}
