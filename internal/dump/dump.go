// Package dump records EGL API responses with personal data removed, so they
// can be shared in bug reports and used as test fixtures.
//
// Redaction is allow-list based: in JSON bodies every string value is replaced
// except under keys known to carry no personal data (units, labels, roles),
// and every number is kept (consumption figures and dates are what we need).
// Non-JSON bodies are not kept at all. Known secrets (username, contract ID,
// tokens) are also scrubbed from URLs.
package dump

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/D34THWINGS/egl2haws/internal/egl"
)

// keepStrings lists keys whose string values are kept as-is.
var keepStrings = map[string]bool{
	"consommation":          true, // the unit, under "unites"
	"libelle":               true,
	"actif":                 true,
	"type":                  true,
	"unite":                 true,
	"statut":                true,
	"code_challenge_method": true,
	"token_type":            true,
}

// dropKeys lists keys whose values are removed entirely, even numbers.
var dropKeys = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "token": true,
	"id": true, "refContrat": true, "reference": true, "numero": true,
	"telephone": true, "codePostal": true, "pce": true, "pdl": true,
}

// Redact returns a redacted copy of a JSON body, or nil if body isn't JSON.
func Redact(body []byte) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return []byte{}
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	out, _ := json.MarshalIndent(redact(v, ""), "", "  ")
	return out
}

func redact(v any, key string) any {
	if dropKeys[key] {
		return "REDACTED"
	}
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			if units, ok := val.(map[string]any); ok && k == "unites" {
				m[k] = units // unit names only, nothing personal
				continue
			}
			m[k] = redact(val, k)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, val := range x {
			l[i] = redact(val, key)
		}
		return l
	case string:
		if keepStrings[key] {
			return x
		}
		return "REDACTED"
	default: // numbers, bools, null
		return x
	}
}

var codeParam = regexp.MustCompile(`(?i)(code|code_challenge|code_verifier)=[^&]*`)

// Recorder writes each response to dir as NN-step.json plus an index.
type Recorder struct {
	Dir     string
	Secrets []string // replaced in URLs (username, contract ID)
	n       int
	index   strings.Builder
	Err     error
}

// Record is suitable for egl.Client.OnResponse.
func (r *Recorder) Record(resp egl.Response) {
	if r.Err != nil {
		return
	}
	r.n++
	name := fmt.Sprintf("%02d-%s.json", r.n, resp.Step)
	body := Redact(resp.Body)
	kept := "redacted JSON"
	if body == nil {
		body, kept = []byte{}, fmt.Sprintf("non-JSON body dropped (%d bytes)", len(resp.Body))
	}
	if err := os.WriteFile(filepath.Join(r.Dir, name), body, 0o600); err != nil {
		r.Err = err
		return
	}
	fmt.Fprintf(&r.index, "%s\t%s %s\tHTTP %d\t%s\n", name, resp.Method, r.scrubURL(resp.URL), resp.Status, kept)
}

// Close writes the index.
func (r *Recorder) Close() error {
	if r.Err != nil {
		return r.Err
	}
	return os.WriteFile(filepath.Join(r.Dir, "index.txt"), []byte(r.index.String()), 0o600)
}

func (r *Recorder) scrubURL(raw string) string {
	s := raw
	for _, secret := range r.Secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "<redacted>")
		s = strings.ReplaceAll(s, url.PathEscape(secret), "<redacted>")
		s = strings.ReplaceAll(s, url.QueryEscape(secret), "<redacted>")
	}
	return codeParam.ReplaceAllString(s, "$1=<redacted>")
}
