package render

import (
	"math"
	"testing"
	"time"
)

func TestOptions_ValidateDefaults(t *testing.T) {
	o := Options{}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if o.Format != "A4" {
		t.Errorf("default format: got %q want A4", o.Format)
	}
	if o.Scale != 1.0 {
		t.Errorf("default scale: got %v want 1.0", o.Scale)
	}
	if o.WaitStrategy != WaitNetworkIdle {
		t.Errorf("default strategy: got %q want %q", o.WaitStrategy, WaitNetworkIdle)
	}
	if o.WaitTimeout != 15*time.Second {
		t.Errorf("default wait timeout: got %v want 15s", o.WaitTimeout)
	}
	if o.Timeout != 30*time.Second {
		t.Errorf("default render timeout: got %v want 30s", o.Timeout)
	}
}

func TestOptions_RejectsBadFormat(t *testing.T) {
	o := Options{Format: "B4"}
	if err := o.Validate(); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestOptions_RejectsSelectorWithoutSelector(t *testing.T) {
	o := Options{WaitStrategy: WaitSelector}
	if err := o.Validate(); err == nil {
		t.Fatal("expected error when selector strategy lacks selector")
	}
}

func TestOptions_RejectsScaleOutOfRange(t *testing.T) {
	for _, s := range []float64{0.05, 2.5} {
		o := Options{Scale: s}
		if err := o.Validate(); err == nil {
			t.Errorf("scale %v should be rejected", s)
		}
	}
}

func TestPaperDimensions_Landscape(t *testing.T) {
	o := Options{Format: "A4", Landscape: true}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	w, h := o.PaperDimensions()
	if w <= h {
		t.Errorf("landscape A4 should have width > height, got w=%v h=%v", w, h)
	}
}

func TestParseMargin(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"0", 0},
		{"10mm", 10.0 / 25.4},
		{"2.54cm", 1.0},
		{"96px", 1.0},
		{"0.5in", 0.5},
		{"1", 1.0},
	}
	for _, tc := range cases {
		got, err := ParseMargin(tc.in)
		if err != nil {
			t.Errorf("ParseMargin(%q) error: %v", tc.in, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("ParseMargin(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseMargin_Rejects(t *testing.T) {
	for _, bad := range []string{"abc", "10kg", "1.2.3mm"} {
		if _, err := ParseMargin(bad); err == nil {
			t.Errorf("ParseMargin(%q) should error", bad)
		}
	}
}
