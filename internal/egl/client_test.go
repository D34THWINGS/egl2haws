package egl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/fake"
)

func TestFakeMatchesClientConstants(t *testing.T) {
	if fake.EGLClientID != clientID || fake.EGLRedirectURI != redirectURI {
		t.Fatal("fake EGL constants out of sync with the client")
	}
}

func newTestClient(f *fake.EGL) *Client {
	c := New(fake.EGLUsername, fake.EGLPassword)
	c.BaseURL = f.URL
	return c
}

func TestFullFlow(t *testing.T) {
	for _, hop := range []bool{false, true} {
		t.Run(fmt.Sprintf("redirectHop=%v", hop), func(t *testing.T) {
			f := fake.NewEGL(t)
			f.RedirectHop(hop)
			f.SetReadings(`{"postes":[{"data":[
				{"consommation":0.123,"annee":2026,"mois":9,"jour":30},
				{"consommation":0.2,"annee":2026,"mois":10,"jour":1},
				{"consommation":null,"annee":2026,"mois":10,"jour":2}
			]}],"unites":{"consommation":"m3"}}`)
			c := newTestClient(f)
			var steps []string
			c.OnResponse = func(r Response) { steps = append(steps, r.Step) }

			got, err := c.Readings(context.Background(), Date{2026, 10, 25}, Date{2026, 11, 3})
			if err != nil {
				t.Fatal(err)
			}
			want := []Reading{
				{Date: Date{2026, 10, 30}, Liters: 123},
				{Date: Date{2026, 11, 1}, Liters: 200},
				{Date: Date{2026, 11, 2}, Liters: 0},
			}
			if len(got) != len(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			for i := range want {
				if got[i].Date != want[i].Date || abs(got[i].Liters-want[i].Liters) > 1e-9 {
					t.Errorf("reading %d: got %v, want %v", i, got[i], want[i])
				}
			}
			q := f.LastQuery()
			if q.Get("dateDebut") != "2026-10-25T00:00:00.000Z" || q.Get("dateFin") != "2026-11-03T00:00:00.000Z" {
				t.Errorf("unexpected range params %v", q)
			}
			if steps[0] != StepLogin || steps[len(steps)-1] != StepReadings {
				t.Errorf("unexpected steps %v", steps)
			}
		})
	}
}

func TestReloginOn401(t *testing.T) {
	f := fake.NewEGL(t)
	c := newTestClient(f)
	ctx := context.Background()
	if _, err := c.Readings(ctx, Date{2026, 1, 1}, Date{2026, 1, 5}); err != nil {
		t.Fatal(err)
	}
	f.ExpireNext()
	if _, err := c.Readings(ctx, Date{2026, 1, 1}, Date{2026, 1, 5}); err != nil {
		t.Fatal(err)
	}
	if f.Logins() != 2 {
		t.Errorf("expected a second login after 401, got %d logins", f.Logins())
	}
}

func TestTokenExpiryTriggersLogin(t *testing.T) {
	f := fake.NewEGL(t)
	c := newTestClient(f)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := c.Readings(ctx, Date{2026, 1, 1}, Date{2026, 1, 2}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := c.Readings(ctx, Date{2026, 1, 1}, Date{2026, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if f.Logins() != 2 {
		t.Errorf("expected re-login after expiry, got %d logins", f.Logins())
	}
}

func TestInvalidCredentials(t *testing.T) {
	c := newTestClient(fake.NewEGL(t))
	c.Password = "wrong"
	err := c.Login(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Step != StepLogin || e.Status != http.StatusUnauthorized {
		t.Fatalf("expected login error, got %v", err)
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestContractSelection(t *testing.T) {
	f := fake.NewEGL(t)
	c := newTestClient(f)
	c.ContractID = "CT2"
	got, err := c.Contract(context.Background())
	if err != nil || got != "CT2" {
		t.Fatalf("got %q, %v", got, err)
	}
	c2 := newTestClient(f)
	c2.ContractID = "nope"
	if _, err := c2.Contract(context.Background()); err == nil {
		t.Fatal("expected unknown contract error")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestMonthBaseIsRememberedAcrossRequests(t *testing.T) {
	f := fake.NewEGL(t)
	c := newTestClient(f)
	ctx := context.Background()
	// Month 12 proves 1-based numbering.
	f.SetReadings(`[{"consommation":1,"annee":2025,"mois":12,"jour":31}]`)
	if _, err := c.Readings(ctx, Date{2025, 12, 1}, Date{2025, 12, 31}); err != nil {
		t.Fatal(err)
	}
	if c.MonthBase == nil || c.MonthBase.ZeroBased {
		t.Fatalf("expected 1-based to be remembered, got %+v", c.MonthBase)
	}
	// Alone, this one-day response is ambiguous and would default to 0-based (Mar 10).
	f.SetReadings(`[{"consommation":1,"annee":2026,"mois":2,"jour":10}]`)
	got, err := c.Readings(ctx, Date{2026, 1, 1}, Date{2026, 3, 31})
	if err != nil || len(got) != 1 || got[0].Date != (Date{2026, 2, 10}) {
		t.Fatalf("got %v, %v", got, err)
	}
}
