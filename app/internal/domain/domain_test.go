package domain

import (
	"errors"
	"testing"
	"time"
)

func TestBilledHours(t *testing.T) {
	cases := []struct {
		stay time.Duration
		want int
	}{
		{0, 1},
		{time.Second, 1},
		{time.Hour, 1},
		{time.Hour + time.Second, 2},
		{150 * time.Minute, 3},
		{-time.Minute, 1},
	}
	for _, c := range cases {
		if got := BilledHours(c.stay); got != c.want {
			t.Errorf("BilledHours(%s) = %d, want %d", c.stay, got, c.want)
		}
	}
}

func TestNewInvoice(t *testing.T) {
	inv := NewInvoice(7, "123ABC02", 90*time.Minute, 500)
	if inv.BilledHours != 2 || inv.TotalKZT != 1000 {
		t.Fatalf("unexpected invoice: %+v", inv)
	}
	if inv.Duration != "1h30m0s" {
		t.Fatalf("unexpected duration: %q", inv.Duration)
	}
}

func TestNormalizePlate(t *testing.T) {
	valid := map[string]string{
		"123abc02":    "123ABC02",
		"  a 123 bc ": "A 123 BC",
		"KZ-777":      "KZ-777",
	}
	for in, want := range valid {
		got, err := NormalizePlate(in)
		if err != nil || got != want {
			t.Errorf("NormalizePlate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", " ", "A", "-AB", "AB-", "AB_12", "ABCDEFGHIJKLMNOPQ", "abc;drop"} {
		if _, err := NormalizePlate(in); !errors.Is(err, ErrInvalidPlate) {
			t.Errorf("NormalizePlate(%q) error = %v, want ErrInvalidPlate", in, err)
		}
	}
}

func TestParseSpotType(t *testing.T) {
	if got, ok := ParseSpotType(" Electric "); !ok || got != SpotElectric {
		t.Fatalf("got %q, %v", got, ok)
	}
	if _, ok := ParseSpotType("helipad"); ok {
		t.Fatal("unknown type must be rejected")
	}
}
