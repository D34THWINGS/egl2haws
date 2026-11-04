package egl

import "testing"

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
