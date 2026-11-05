package egl

import (
	"os"
	"testing"
)

func TestParseReadingsMonthBase(t *testing.T) {
	from, to := Date{2026, 1, 20}, Date{2026, 2, 10}
	tests := []struct {
		name string
		body string
		want []Date
	}{
		{
			name: "zero-based detected from month 0",
			body: `[{"consommation":1,"annee":2026,"mois":0,"jour":31},{"consommation":1,"annee":2026,"mois":1,"jour":1}]`,
			want: []Date{{2026, 1, 31}, {2026, 2, 1}},
		},
		{
			name: "one-based detected from month 12",
			body: `[{"consommation":1,"annee":2025,"mois":12,"jour":31},{"consommation":1,"annee":2026,"mois":1,"jour":1}]`,
			want: []Date{{2025, 12, 31}, {2026, 1, 1}},
		},
		{
			name: "one-based detected from requested range",
			body: `[{"consommation":1,"annee":2026,"mois":1,"jour":30},{"consommation":1,"annee":2026,"mois":2,"jour":2}]`,
			want: []Date{{2026, 1, 30}, {2026, 2, 2}},
		},
		{
			name: "zero-based detected from requested range",
			// Read as 1-based these are Jan 5/8, outside the requested
			// range; read as 0-based they're Feb 5/8, inside it.
			body: `[{"consommation":1,"annee":2026,"mois":1,"jour":5},{"consommation":1,"annee":2026,"mois":1,"jour":8}]`,
			want: []Date{{2026, 2, 5}, {2026, 2, 8}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseReadings([]byte(tt.body), from, to)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v", got)
			}
			for i, d := range tt.want {
				if got[i].Date != d {
					t.Errorf("reading %d: got %s, want %s", i, got[i].Date, d)
				}
			}
		})
	}
}

func TestParseReadingsUnitsAndErrors(t *testing.T) {
	from, to := Date{2026, 3, 1}, Date{2026, 3, 31}
	got, err := ParseReadings([]byte(`{"postes":[{"data":[{"consommation":154,"annee":2026,"mois":2,"jour":3}]}],"unites":{"consommation":"L"}}`), from, to)
	if err != nil || len(got) != 1 || got[0].Liters != 154 || got[0].Date != (Date{2026, 3, 3}) {
		t.Fatalf("liters: got %v, %v", got, err)
	}
	if got, err := ParseReadings([]byte(`{"postes":[]}`), from, to); err != nil || got != nil {
		t.Fatalf("empty: got %v, %v", got, err)
	}
	for name, body := range map[string]string{
		"bad unit":     `{"postes":[{"data":[{"consommation":1,"annee":2026,"mois":2,"jour":3}]}],"unites":{"consommation":"gal"}}`,
		"negative":     `[{"consommation":-1,"annee":2026,"mois":2,"jour":3}]`,
		"invalid date": `[{"consommation":1,"annee":2026,"mois":13,"jour":40}]`,
		"not json":     `<html>maintenance</html>`,
	} {
		if _, err := ParseReadings([]byte(body), from, to); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDate(t *testing.T) {
	d := Date{2026, 2, 28}
	if d.AddDays(1) != (Date{2026, 3, 1}) || d.AddDays(-59) != (Date{2025, 12, 31}) {
		t.Fatal("AddDays")
	}
	if !d.Before(Date{2026, 3, 1}) || d.Before(d) {
		t.Fatal("Before")
	}
	if d.String() != "2026-02-28" {
		t.Fatal("String")
	}
}

// TestParseLiveFormat uses the shape of a real consommationsJournalieres
// response (2026-11, volumes replaced with synthetic ones): litres, 0-based
// months, extra fields (index, debitMin, volumeEstimeFuite).
func TestParseLiveFormat(t *testing.T) {
	body, err := os.ReadFile("testdata/consommationsJournalieres.json")
	if err != nil {
		t.Fatal(err)
	}
	got, base, units, err := parseReadings(body, Date{2026, 9, 20}, Date{2026, 11, 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if units.Consommation != "l" {
		t.Errorf("units %+v", units)
	}
	// The fixture's leak-field units were redacted: defaults apply (L/h, L).
	for _, r := range got {
		if r.MinFlowLH == nil || *r.MinFlowLH != 0 || r.LeakLiters == nil || *r.LeakLiters != 0 {
			t.Fatalf("leak fields not parsed on %s: %+v", r.Date, r)
		}
	}
	if !base.ZeroBased || !base.Decisive {
		t.Errorf("month base %+v, want decisive 0-based (mois 9 + jour 31 only exists as October)", base)
	}
	if len(got) != 42 || got[0].Date != (Date{2026, 9, 21}) || got[len(got)-1].Date != (Date{2026, 11, 1}) {
		t.Fatalf("got %d readings from %v to %v", len(got), got[0].Date, got[len(got)-1].Date)
	}
	total := 0.0
	byDay := map[Date]float64{}
	for i, r := range got {
		total += r.Liters
		byDay[r.Date] = r.Liters
		if i > 0 && r.Date != got[i-1].Date.AddDays(1) {
			t.Errorf("gap between %s and %s", got[i-1].Date, r.Date)
		}
	}
	if got[0].Liters != 387 || byDay[Date{2026, 10, 31}] != 112 || got[len(got)-1].Liters != 107 || total != 9281 {
		t.Errorf("unexpected volumes: first=%v oct31=%v last=%v total=%v", got[0].Liters, byDay[Date{2026, 10, 31}], got[len(got)-1].Liters, total)
	}
}

func TestParseLeakFields(t *testing.T) {
	from, to := Date{2026, 3, 1}, Date{2026, 3, 31}
	body := `{"postes":[{"data":[
		{"consommation":0.5,"debitMin":0.012,"volumeEstimeFuite":0.288,"annee":2026,"mois":2,"jour":3},
		{"consommation":0.1,"annee":2026,"mois":2,"jour":4}
	]}],"unites":{"consommation":"m3","debitMin":"m3/h","volumeEstimeFuite":"m3"}}`
	got, err := ParseReadings([]byte(body), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].MinFlowLH == nil || *got[0].MinFlowLH != 12 || got[0].LeakLiters == nil || *got[0].LeakLiters != 288 {
		t.Errorf("day 1: %+v", got[0])
	}
	if got[1].MinFlowLH != nil || got[1].LeakLiters != nil {
		t.Errorf("day 2 should have no leak fields: %+v", got[1])
	}
	for unit, want := range map[string]float64{"l/h": 1, "L/h": 1, "m³/h": 1000, "l/min": 60, "": 1, "REDACTED": 1} {
		if f := flowLHFactor(unit); f != want {
			t.Errorf("flowLHFactor(%q) = %v, want %v", unit, f, want)
		}
	}
	// A negative min flow is ignored rather than failing the sync.
	got, err = ParseReadings([]byte(`[{"consommation":1,"debitMin":-1,"annee":2026,"mois":2,"jour":3}]`), from, to)
	if err != nil || got[0].MinFlowLH != nil {
		t.Errorf("negative debitMin: %+v, %v", got, err)
	}
}
