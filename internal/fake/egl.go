// Package fake provides in-process fakes of the EGL website and Home
// Assistant's websocket API, for tests.
package fake

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Must match the real values used by the egl package.
const (
	EGLClientID    = "kwnOk0B_aqlOI6p_GVxrbf6"
	EGLRedirectURI = "https://agence.eaudugrandlyon.com/autorisation-callback.html"
	EGLUsername    = "me@example.com"
	EGLPassword    = "secret"
	EGLContract    = "CT.opaque/1"
)

// EGL mimics the customer area closely enough to exercise the full flow:
// session cookie after login, role activation, PKCE-checked code exchange,
// Bearer-protected data endpoints.
type EGL struct {
	URL string // application root, to use as egl.Client.BaseURL

	mu          sync.Mutex
	challenge   string
	roleActive  bool
	tokens      map[string]bool
	logins      int
	expireNext  bool
	redirectHop bool
	readings    string
	days        map[string]float64 // YYYY-MM-DD -> m³, served 0-based like the live API
	minFlows    map[string]float64 // YYYY-MM-DD -> debitMin, l/h
	leaks       map[string]float64 // YYYY-MM-DD -> volumeEstimeFuite, l
	lastQuery   url.Values
}

// NewEGL starts a fake EGL server, closed when the test ends.
func NewEGL(t testing.TB) *EGL {
	f := &EGL{tokens: map[string]bool{}, readings: `{"postes":[{"data":[]}]}`}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL + "/application"
	return f
}

// SetReadings sets a fixed consommationsJournalieres response body,
// returned whatever range is requested.
func (f *EGL) SetReadings(body string) { f.mu.Lock(); f.readings, f.days = body, nil; f.mu.Unlock() }

// SetDays serves daily volumes (YYYY-MM-DD -> m³), filtered by the requested
// range and encoded like the live API (0-based months, m³).
func (f *EGL) SetDays(days map[string]float64) { f.mu.Lock(); f.days = days; f.mu.Unlock() }

// SetLeakFields adds debitMin (L/h) and volumeEstimeFuite (L) to the days
// served by SetDays. Days missing from both maps get 0 for both, like the
// live API.
func (f *EGL) SetLeakFields(minFlows, leaks map[string]float64) {
	f.mu.Lock()
	f.minFlows, f.leaks = minFlows, leaks
	f.mu.Unlock()
}

func (f *EGL) daysBody(q url.Values) string {
	from, _ := time.Parse("2006-01-02T15:04:05.000Z", q.Get("dateDebut"))
	to, _ := time.Parse("2006-01-02T15:04:05.000Z", q.Get("dateFin"))
	var entries []string
	for day, m3 := range f.days {
		d, err := time.Parse("2006-01-02", day)
		if err != nil || d.Before(from) || d.After(to) {
			continue
		}
		entries = append(entries, fmt.Sprintf(`{"consommation":%g,"debitMin":%g,"volumeEstimeFuite":%g,"annee":%d,"mois":%d,"jour":%d}`,
			m3, f.minFlows[day], f.leaks[day], d.Year(), int(d.Month())-1, d.Day()))
	}
	return `{"postes":[{"data":[` + strings.Join(entries, ",") + `]}],"unites":{"consommation":"m3","debitMin":"l/h","volumeEstimeFuite":"l"}}`
}

// ExpireNext makes the next authenticated call fail with 401.
func (f *EGL) ExpireNext() { f.mu.Lock(); f.expireNext = true; f.mu.Unlock() }

// RedirectHop adds an intermediate redirect before the OAuth2 callback.
func (f *EGL) RedirectHop(on bool) { f.mu.Lock(); f.redirectHop = on; f.mu.Unlock() }

// Logins returns how many successful logins happened.
func (f *EGL) Logins() int { f.mu.Lock(); defer f.mu.Unlock(); return f.logins }

// LastQuery returns the query of the last readings request.
func (f *EGL) LastQuery() url.Values { f.mu.Lock(); defer f.mu.Unlock(); return f.lastQuery }

func (f *EGL) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Entreprise") != "EPGL" {
		http.Error(w, "missing entreprise header", http.StatusBadRequest)
		return
	}
	hasSession := func() bool {
		c, err := r.Cookie("SESSION")
		return err == nil && c.Value == "s1"
	}
	switch {
	case r.URL.Path == "/application/auth/externe/authentification" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		if r.Form.Get("username") != EGLUsername || r.Form.Get("password") != EGLPassword || r.Form.Get("client_id") != EGLClientID {
			http.Error(w, `{"message":"Identifiants invalides"}`, http.StatusUnauthorized)
			return
		}
		f.logins++
		http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "s1", Path: "/"})
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/application/auth/externe/utilisateur/roles":
		if !hasSession() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPut {
			var roles []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&roles); err != nil || len(roles) == 0 || roles[0]["id"] != "client" {
				http.Error(w, "bad roles", http.StatusBadRequest)
				return
			}
			f.roleActive = true
		}
		fmt.Fprint(w, `[{"id":"client","libelle":"client","actif":true}]`)
	case r.URL.Path == "/application/auth/authorize-internet":
		if !hasSession() || !f.roleActive {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != EGLRedirectURI {
			http.Error(w, "bad authorize params", http.StatusBadRequest)
			return
		}
		f.challenge = q.Get("code_challenge")
		if f.redirectHop {
			http.Redirect(w, r, "/application/auth/hop", http.StatusFound)
			return
		}
		http.Redirect(w, r, EGLRedirectURI+"?code=abc123", http.StatusFound)
	case r.URL.Path == "/application/auth/hop":
		http.Redirect(w, r, EGLRedirectURI+"?code=abc123", http.StatusFound)
	case r.URL.Path == "/application/auth/tokenUtilisateurInternet" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "abc123" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		tok := fmt.Sprintf("tok%d", f.logins)
		f.tokens[tok] = true
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, tok)
	case strings.HasPrefix(r.URL.Path, "/application/rest/"):
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !f.tokens[tok] || f.expireNext {
			f.expireNext = false
			delete(f.tokens, tok)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/application/rest/produits/contrats":
			fmt.Fprintf(w, `[{"id":%q,"refContrat":"123"},{"id":"CT2"}]`, EGLContract)
		case "/application/rest/produits/contrats/" + EGLContract + "/consommationsJournalieres":
			f.lastQuery = r.URL.Query()
			if f.days != nil {
				fmt.Fprint(w, f.daysBody(f.lastQuery))
			} else {
				fmt.Fprint(w, f.readings)
			}
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}
