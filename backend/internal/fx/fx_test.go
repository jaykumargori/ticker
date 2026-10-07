package fx

import "testing"

func TestSanitize(t *testing.T) {
	p := provider{name: "test", home: "x"}
	s, err := sanitize(map[string]float64{"INR": 96.4, "EUR": 0.88, "XYZ": 5, "GBP": -1, "JPY": 0}, p, "2026-10-06")
	if err != nil {
		t.Fatal(err)
	}
	if s.Rates["USD"] != 1 || s.Rates["INR"] != 96.4 || s.Rates["EUR"] != 0.88 {
		t.Fatalf("rates = %v", s.Rates)
	}
	for _, bad := range []string{"XYZ", "GBP", "JPY"} {
		if _, ok := s.Rates[bad]; ok {
			t.Fatalf("%s should be dropped: %v", bad, s.Rates)
		}
	}
	if _, err := sanitize(map[string]float64{"EUR": 0.9}, p, ""); err == nil {
		t.Fatal("expected error when INR missing")
	}
}
