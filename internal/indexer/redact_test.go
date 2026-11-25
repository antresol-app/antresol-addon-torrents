package indexer

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// lastError (humanError) never carries tracker credentials, even when the
// Russian detail after a sentinel quotes a URL.
func TestHumanErrorRedactsSecrets(t *testing.T) {
	err := fmt.Errorf("%w: сайт вернул https://t.example/login?passkey=SECRET1", cardigann.ErrLoginFailed)
	if got := humanError(err); strings.Contains(got, "SECRET1") || !strings.Contains(got, "Не удалось войти") {
		t.Fatalf("%s", got)
	}
}

// A text field by a credential's name is secret too (the admin API shows
// it as hidden), a checkbox of the same word is not.
func TestSecretByNameNeedsATextField(t *testing.T) {
	const yml = `id: s
name: S
links: ["https://s.example/"]
settings:
  - {name: pin, type: text, label: PIN}
  - {name: staffpass, type: text, label: Staff}
  - {name: csrf_token, label: CSRF}
  - {name: usetoken, type: checkbox, label: Use tokens}
  - {name: sort, type: select, label: Sort, options: {a: A}}
search:
  path: s
  rows: {selector: tr}
  fields: {title: {selector: td}}
`
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "s.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	def, err := e.cat.Definition("s")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"pin": true, "staffpass": true, "csrf_token": true, "usetoken": false, "sort": false, "unknown": false, "password": true} {
		if got := isSecret(def, name); got != want {
			t.Errorf("isSecret(%q) = %v, want %v", name, got, want)
		}
	}
	got := e.m.Redact(Source{DefinitionID: "s", Settings: map[string]string{"pin": "1234", "usetoken": "true", "sort": "a"}})
	if got.Settings["pin"] != RedactedValue || got.Settings["usetoken"] != "true" || got.Settings["sort"] != "a" {
		t.Fatalf("redacted settings %v", got.Settings)
	}
	for _, s := range def.Settings {
		spec, _ := settingSpec(def, s)
		if spec.Secret != (s.Name == "pin" || s.Name == "staffpass" || s.Name == "csrf_token") {
			t.Errorf("%s: Secret = %v", s.Name, spec.Secret)
		}
	}
}

// OPDS failures are logged without the credentials of the catalog's URL.
func TestOPDSLogRedactsURLTokens(t *testing.T) {
	e := newEnv(t)
	e.reload()
	var buf strings.Builder
	e.m.log = slog.New(slog.NewTextHandler(&buf, nil))
	e.addOPDS("http://127.0.0.1:1/opds?token=SECRETX&format=atom", nil)
	if !strings.Contains(buf.String(), "OPDS source test failed") {
		t.Fatalf("nothing was logged: %q", buf.String())
	}
	if strings.Contains(buf.String(), "SECRETX") {
		t.Fatalf("the log holds the token: %s", buf.String())
	}
}
