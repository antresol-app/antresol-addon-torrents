package coreadapt_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol/addonkit"
)

// fitCore answers GET /voiceover/fit?path= from states (a path not there:
// 404) and records each POST body in queued.
func fitCore(t *testing.T, states map[string]addonkit.FitState, queued *[]map[string]any) coreadapt.VoiceOver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/addon-core/v1/voiceover/fit" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			*queued = append(*queued, b)
			w.WriteHeader(http.StatusAccepted)
		case http.MethodGet:
			f, ok := states[r.URL.Query().Get("path")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"Озвучка не найдена"}`))
				return
			}
			json.NewEncoder(w).Encode(f)
		}
	}))
	t.Cleanup(srv.Close)
	return coreadapt.VoiceOver{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})}
}

// TestFitState_Mapped: the core's neutral states become acquire's
// (ok → fitted with the worker's offset as it is: a track 1.5 s late fits
// at -1500 ms); a path the core has not queued is not known.
func TestFitState_Mapped(t *testing.T) {
	var queued []map[string]any
	v := fitCore(t, map[string]addonkit.FitState{
		"/m/p.mka": {State: addonkit.FitPending},
		"/m/o.mka": {State: addonkit.FitOK, OffsetMs: -1500, Tempo: 1},
		"/m/x.mka": {State: addonkit.FitMisfit},
		"/m/u.mka": {State: addonkit.FitUnmeasured},
	}, &queued)
	type st struct {
		state         string
		offset, tempo float64
		ok            bool
	}
	for path, want := range map[string]st{
		"/m/p.mka":    {coreadapt.VoicePlaced, 0, 0, true},
		"/m/o.mka":    {coreadapt.VoiceFitted, -1500, 1, true},
		"/m/x.mka":    {coreadapt.VoiceMisfit, 0, 0, true},
		"/m/u.mka":    {coreadapt.VoiceUnmeasured, 0, 0, true},
		"/m/none.mka": {"", 0, 0, false},
	} {
		var got st
		got.state, got.offset, got.tempo, got.ok = v.State(ctx, path)
		if got != want {
			t.Errorf("%s: %+v, want %+v", path, got, want)
		}
	}
	if err := v.Placed(ctx, "/m/S01E01.ru.LostFilm.mka", 7, "en"); err != nil {
		t.Fatal(err)
	}
	if want := []map[string]any{{"path": "/m/S01E01.ru.LostFilm.mka", "fileId": float64(7), "origLang": "en"}}; !reflect.DeepEqual(queued, want) {
		t.Fatalf("queued %v", queued)
	}
}

// TestAliases_Adapter: the golden item's aliases come through
// (none: []); an unknown item is library.ErrNotFound.
func TestAliases_Adapter(t *testing.T) {
	l := lib(t)
	al, err := l.Aliases(ctx, 1)
	if err != nil || al == nil || len(al) != 0 {
		t.Fatalf("aliases %#v %v", al, err)
	}
	if _, err := l.Aliases(ctx, 99); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// an item with alias titles: as the core lists them
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":4,"kind":"series","title":"Миллиарды","originalTitle":"Billions","aliases":["Деньги","Billions: Деньги"]}`))
	}))
	defer srv.Close()
	l = coreadapt.Library{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})}
	if al, err := l.Aliases(ctx, 4); err != nil || !reflect.DeepEqual(al, []string{"Деньги", "Billions: Деньги"}) {
		t.Fatalf("aliases %#v %v", al, err)
	}
}

// Placed and State over the core's recorded answers: the queued track is
// placed (pending) until the worker has fitted it.
func TestVoiceOver_Replayed(t *testing.T) {
	v := coreadapt.VoiceOver{C: replay(t)}
	const dubbed = "{root}/movies/Матрица (1999)/Матрица (1999).rus.mka"
	if err := v.Placed(ctx, dubbed, 1, "en"); err != nil {
		t.Fatal(err)
	}
	if st, off, tempo, ok := v.State(ctx, dubbed); !ok || st != coreadapt.VoicePlaced || off != 0 || tempo != 0 {
		t.Fatalf("state %q %v %v %v", st, off, tempo, ok)
	}
}
