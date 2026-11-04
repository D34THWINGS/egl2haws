// Package egl talks to the Eau du Grand Lyon customer area
// (agence.eaudugrandlyon.com). There is no public API: this mirrors the calls
// the website itself makes, as documented by the ha-egl-water project
// (https://github.com/aldweb/ha-egl-water, MIT).
//
// Login is a home-made OAuth2 PKCE flow:
//
//  1. POST /auth/externe/authentification  -> session cookie
//  2. GET  /auth/externe/utilisateur/roles
//  3. PUT  /auth/externe/utilisateur/roles  -> activate the "client" role
//  4. GET  /auth/externe/utilisateur/roles  -> confirm
//  5. GET  /auth/authorize-internet         -> OAuth2 code (redirect)
//  6. POST /auth/tokenUtilisateurInternet   -> Bearer token
//
// Data then comes from /rest/produits/contrats[/{id}/consommationsJournalieres].
package egl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the customer area application root.
	DefaultBaseURL = "https://agence.eaudugrandlyon.com/application"

	clientID         = "kwnOk0B_aqlOI6p_GVxrbf6"
	entrepriseHeader = "EPGL"
	redirectURI      = "https://agence.eaudugrandlyon.com/autorisation-callback.html"

	maxBody      = 10 << 20
	maxRedirects = 5
	dateLayout   = "2006-01-02T15:04:05.000Z"
)

// Steps, used in errors and logs so a failure points at the exact call.
const (
	StepLogin     = "login"
	StepRoles     = "roles"
	StepAuthorize = "authorize"
	StepToken     = "token"
	StepContracts = "contracts"
	StepReadings  = "readings"
)

// ErrInvalidCredentials is wrapped when EGL rejects the username/password.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Error describes a failed call to EGL.
type Error struct {
	Step   string // one of the Step* constants
	Status int    // HTTP status, 0 if the request itself failed
	Err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("egl %s: HTTP %d: %v", e.Step, e.Status, e.Err)
	}
	return fmt.Sprintf("egl %s: %v", e.Step, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Response is passed to Client.OnResponse for every HTTP response, so callers
// can record real API payloads (see the dump command).
type Response struct {
	Step   string
	Method string
	URL    string
	Status int
	Body   []byte
}

// Client is an EGL API client. It is not safe for concurrent use.
type Client struct {
	BaseURL  string
	Username string
	Password string
	// ContractID selects a contract when the account has several. Empty means
	// the first one.
	ContractID string
	// OnResponse, if set, is called with every response received.
	OnResponse func(Response)

	// MonthBase is set once a response has shown how months are numbered;
	// later responses are parsed the same way.
	MonthBase *MonthBase

	http     *http.Client
	token    string
	expiry   time.Time
	contract string
	now      func() time.Time
}

// New returns a client for the given account.
func New(username, password string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		BaseURL:  DefaultBaseURL,
		Username: username,
		Password: password,
		http: &http.Client{
			Jar:     jar,
			Timeout: 60 * time.Second,
			// Redirects are followed by hand: the OAuth2 code is read from
			// the Location header.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now,
	}
}

// Login runs the full authentication flow and stores the Bearer token.
func (c *Client) Login(ctx context.Context) error {
	form := url.Values{"username": {c.Username}, "password": {c.Password}, "client_id": {clientID}}
	status, body, err := c.do(ctx, StepLogin, http.MethodPost, c.BaseURL+"/auth/externe/authentification", formBody(form), "application/x-www-form-urlencoded", "")
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		e := fmt.Errorf("unexpected response: %s", snippet(body))
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest {
			e = fmt.Errorf("%w: %s", ErrInvalidCredentials, snippet(body))
		}
		return &Error{Step: StepLogin, Status: status, Err: e}
	}

	rolesURL := c.BaseURL + "/auth/externe/utilisateur/roles"
	if err := c.expect(ctx, StepRoles, http.MethodGet, rolesURL, nil, "", http.StatusOK); err != nil {
		return err
	}
	roles := []byte(`[{"id":"client","libelle":"client","actif":false}]`)
	if err := c.expect(ctx, StepRoles, http.MethodPut, rolesURL, roles, "application/json", http.StatusOK, http.StatusNoContent); err != nil {
		return err
	}
	if err := c.expect(ctx, StepRoles, http.MethodGet, rolesURL, nil, "", http.StatusOK); err != nil {
		return err
	}

	verifier, challenge, err := newPKCE()
	if err != nil {
		return &Error{Step: StepAuthorize, Err: err}
	}
	code, err := c.authorize(ctx, challenge)
	if err != nil {
		return err
	}

	form = url.Values{
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	status, body, err = c.do(ctx, StepToken, http.MethodPost, c.BaseURL+"/auth/tokenUtilisateurInternet", formBody(form), "application/x-www-form-urlencoded", "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &Error{Step: StepToken, Status: status, Err: fmt.Errorf("unexpected response: %s", snippet(body))}
	}
	var tok struct {
		AccessToken string  `json:"access_token"`
		Token       string  `json:"token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return &Error{Step: StepToken, Status: status, Err: fmt.Errorf("decode: %w", err)}
	}
	c.token = tok.AccessToken
	if c.token == "" {
		c.token = tok.Token
	}
	if c.token == "" {
		return &Error{Step: StepToken, Status: status, Err: errors.New("no access_token in response")}
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	c.expiry = c.now().Add(time.Duration(tok.ExpiresIn)*time.Second - time.Minute)
	return nil
}

var codeInBody = []*regexp.Regexp{
	regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`[?&]code=([A-Za-z0-9._~-]+)`),
}

// authorize requests an OAuth2 code. The server answers with a redirect to
// redirect_uri?code=..., possibly after intermediate hops, or with the code in
// a 200 body.
func (c *Client) authorize(ctx context.Context, challenge string) (string, error) {
	q := url.Values{
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"client_id":             {clientID},
	}
	next := c.BaseURL + "/auth/authorize-internet?" + q.Encode()
	for hop := 0; hop <= maxRedirects; hop++ {
		resp, body, err := c.raw(ctx, StepAuthorize, http.MethodGet, next, nil, "", "")
		if err != nil {
			return "", err
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			u, err := resp.Request.URL.Parse(loc)
			if err != nil {
				return "", &Error{Step: StepAuthorize, Status: resp.StatusCode, Err: fmt.Errorf("bad Location %q: %w", loc, err)}
			}
			if code := u.Query().Get("code"); code != "" {
				return code, nil
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				next = u.String()
				continue
			}
		}
		if code := resp.Request.URL.Query().Get("code"); code != "" {
			return code, nil
		}
		if resp.StatusCode == http.StatusOK {
			for _, re := range codeInBody {
				if m := re.FindSubmatch(body); m != nil {
					return string(m[1]), nil
				}
			}
		}
		return "", &Error{Step: StepAuthorize, Status: resp.StatusCode, Err: fmt.Errorf("no OAuth2 code in response: %s", snippet(body))}
	}
	return "", &Error{Step: StepAuthorize, Err: errors.New("too many redirects")}
}

func (c *Client) ensureToken(ctx context.Context) error {
	if c.token != "" && c.now().Before(c.expiry) {
		return nil
	}
	return c.Login(ctx)
}

// authGet performs an authenticated GET, logging in again once on 401.
func (c *Client) authGet(ctx context.Context, step, u string) (int, []byte, error) {
	if err := c.ensureToken(ctx); err != nil {
		return 0, nil, err
	}
	status, body, err := c.do(ctx, step, http.MethodGet, u, nil, "", c.token)
	if err == nil && status == http.StatusUnauthorized {
		c.token = ""
		if err := c.ensureToken(ctx); err != nil {
			return 0, nil, err
		}
		status, body, err = c.do(ctx, step, http.MethodGet, u, nil, "", c.token)
	}
	return status, body, err
}

// Contract returns the identifier used in data URLs, selecting ContractID if
// set, else the first contract on the account.
func (c *Client) Contract(ctx context.Context) (string, error) {
	if c.contract != "" {
		return c.contract, nil
	}
	status, body, err := c.authGet(ctx, StepContracts, c.BaseURL+"/rest/produits/contrats")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &Error{Step: StepContracts, Status: status, Err: fmt.Errorf("unexpected response: %s", snippet(body))}
	}
	ids, err := parseContracts(body)
	if err != nil {
		return "", &Error{Step: StepContracts, Status: status, Err: err}
	}
	if len(ids) == 0 {
		return "", &Error{Step: StepContracts, Status: status, Err: errors.New("no contract on this account")}
	}
	c.contract = ids[0]
	if c.ContractID != "" {
		c.contract = ""
		for _, id := range ids {
			if id == c.ContractID {
				c.contract = id
			}
		}
		if c.contract == "" {
			return "", &Error{Step: StepContracts, Status: status, Err: fmt.Errorf("contract %q not found (account has %d)", c.ContractID, len(ids))}
		}
	}
	return c.contract, nil
}

func parseContracts(body []byte) ([]string, error) {
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		var wrapped struct {
			Contrats []map[string]any `json:"contrats"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		list = wrapped.Contrats
	}
	var ids []string
	for i, ct := range list {
		id := ""
		for _, k := range []string{"id", "token", "refContrat"} {
			if v, ok := ct[k]; ok && v != nil && fmt.Sprint(v) != "" {
				id = fmt.Sprint(v)
				break
			}
		}
		if id == "" {
			return nil, fmt.Errorf("contract #%d has no id/token/refContrat field", i)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Readings returns daily consumption for the days in [from, to] (inclusive,
// calendar dates). Call it with at most ~90 days at a time.
func (c *Client) Readings(ctx context.Context, from, to Date) ([]Reading, error) {
	contract, err := c.Contract(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"dateDebut": {from.Time().UTC().Format(dateLayout)},
		"dateFin":   {to.Time().UTC().Format(dateLayout)},
	}
	u := c.BaseURL + "/rest/produits/contrats/" + url.PathEscape(contract) + "/consommationsJournalieres?" + q.Encode()
	status, body, err := c.authGet(ctx, StepReadings, u)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &Error{Step: StepReadings, Status: status, Err: fmt.Errorf("unexpected response: %s", snippet(body))}
	}
	readings, base, err := parseReadings(body, from, to, c.MonthBase)
	if err != nil {
		return nil, &Error{Step: StepReadings, Status: status, Err: err}
	}
	if c.MonthBase == nil && base.Decisive {
		c.MonthBase = &base
	}
	return readings, nil
}

func (c *Client) expect(ctx context.Context, step, method, u string, body []byte, contentType string, ok ...int) error {
	status, resp, err := c.do(ctx, step, method, u, body, contentType, "")
	if err != nil {
		return err
	}
	for _, s := range ok {
		if status == s {
			return nil
		}
	}
	return &Error{Step: step, Status: status, Err: fmt.Errorf("%s %s: unexpected response: %s", method, path(u), snippet(resp))}
}

func (c *Client) do(ctx context.Context, step, method, u string, body []byte, contentType, bearer string) (int, []byte, error) {
	resp, b, err := c.raw(ctx, step, method, u, body, contentType, bearer)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

func (c *Client) raw(ctx context.Context, step, method, u string, body []byte, contentType, bearer string) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, nil, &Error{Step: step, Err: err}
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "fr, fr-FR;q=0.9")
	req.Header.Set("Entreprise", entrepriseHeader)
	req.Header.Set("User-Agent", "egl2haws (+https://github.com/D34THWINGS/egl2haws)")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, &Error{Step: step, Err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, &Error{Step: step, Status: resp.StatusCode, Err: fmt.Errorf("read body: %w", err)}
	}
	if c.OnResponse != nil {
		c.OnResponse(Response{Step: step, Method: method, URL: u, Status: resp.StatusCode, Body: b})
	}
	return resp, b, nil
}

func newPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func formBody(v url.Values) []byte { return []byte(v.Encode()) }

func path(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Path
	}
	return u
}

// snippet keeps error messages short and on one line.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		s = "(empty body)"
	}
	return s
}
