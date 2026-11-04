package dump

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/D34THWINGS/egl2haws/internal/egl"
)

func TestRedactKeepsReadingsDropsPersonalData(t *testing.T) {
	body := []byte(`{
		"postes":[{"data":[{"consommation":0.154,"annee":2026,"mois":9,"jour":30}]}],
		"unites":{"consommation":"m3"},
		"titulaire":{"nom":"Dupont","email":"jd@example.com"},
		"id":"CT.secret", "refContrat": 12345678
	}`)
	out := string(Redact(body))
	for _, want := range []string{`"consommation": 0.154`, `"annee": 2026`, `"mois": 9`, `"consommation": "m3"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	for _, leak := range []string{"Dupont", "jd@example.com", "CT.secret", "12345678"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %s in\n%s", leak, out)
		}
	}
	if Redact([]byte("<html>")) != nil {
		t.Error("non-JSON should be dropped")
	}
}

func TestRecorderScrubsURLs(t *testing.T) {
	dir := t.TempDir()
	r := &Recorder{Dir: dir, Secrets: []string{"CT.opaque/1", "me@example.com"}}
	r.Record(egl.Response{Step: "readings", Method: "GET", URL: "https://x/rest/produits/contrats/CT.opaque%2F1/consommationsJournalieres?dateDebut=a", Status: 200, Body: []byte(`[]`)})
	r.Record(egl.Response{Step: "authorize", Method: "GET", URL: "https://x/auth/authorize-internet?code_challenge=abc&client_id=x", Status: 302, Body: []byte("<html>")})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	idx, _ := os.ReadFile(filepath.Join(dir, "index.txt"))
	s := string(idx)
	if strings.Contains(s, "CT.opaque") || strings.Contains(s, "code_challenge=abc") {
		t.Errorf("index leaks secrets:\n%s", s)
	}
	if !strings.Contains(s, "01-readings.json") || !strings.Contains(s, "non-JSON body dropped") {
		t.Errorf("unexpected index:\n%s", s)
	}
}
