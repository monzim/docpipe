// PDF rendering options. These map a request payload onto chromedp's
// page.PrintToPDF call, plus the wait-for-render strategy used before
// printing.
package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Paper sizes — width × height in inches. Values match Chrome defaults.
var paperSizes = map[string][2]float64{
	"A3":     {11.69, 16.54},
	"A4":     {8.27, 11.69},
	"A5":     {5.83, 8.27},
	"LETTER": {8.5, 11.0},
	"LEGAL":  {8.5, 14.0},
}

// Margin in inches on each side. Zero means no margin.
type Margin struct {
	Top, Right, Bottom, Left float64
}

// WaitStrategy selects the readiness signal before invoking Print.
//
//	WaitLoad        — Page.loadEventFired
//	WaitNetworkIdle — load + 500 ms of no in-flight network requests
//	WaitSelector    — load + WaitVisible on Selector
//	WaitNone        — emit PDF immediately after navigation
type WaitStrategy string

const (
	WaitLoad        WaitStrategy = "load"
	WaitNetworkIdle WaitStrategy = "networkidle"
	WaitSelector    WaitStrategy = "selector"
	WaitNone        WaitStrategy = "none"
)

// Options is the rendered PDF configuration. Defaults are applied by Validate
// so callers can pass an almost-empty struct.
type Options struct {
	Format              string
	Landscape           bool
	Scale               float64
	PrintBackground     bool
	PreferCSSPageSize   bool
	Margin              Margin
	HeaderTemplate      string
	FooterTemplate      string
	DisplayHeaderFooter bool
	PageRanges          string

	WaitStrategy WaitStrategy
	WaitTimeout  time.Duration
	WaitSelector string

	// Timeout bounds the entire render including navigation, wait, and print.
	Timeout time.Duration
}

// Validate sets defaults and rejects illegal combinations. Returns a wrapped
// error so callers can surface the offending field.
func (o *Options) Validate() error {
	if o.Format == "" {
		o.Format = "A4"
	}
	o.Format = strings.ToUpper(o.Format)
	if _, ok := paperSizes[o.Format]; !ok {
		return fmt.Errorf("unsupported paper format %q (allowed: A3, A4, A5, Letter, Legal)", o.Format)
	}
	if o.Scale == 0 {
		o.Scale = 1.0
	}
	if o.Scale < 0.1 || o.Scale > 2.0 {
		return fmt.Errorf("scale=%v out of allowed range 0.1..2.0", o.Scale)
	}
	if o.WaitStrategy == "" {
		o.WaitStrategy = WaitNetworkIdle
	}
	switch o.WaitStrategy {
	case WaitLoad, WaitNetworkIdle, WaitSelector, WaitNone:
	default:
		return fmt.Errorf("unsupported wait strategy %q", o.WaitStrategy)
	}
	if o.WaitStrategy == WaitSelector && o.WaitSelector == "" {
		return fmt.Errorf(`wait.strategy="selector" requires wait.selector`)
	}
	if o.WaitTimeout == 0 {
		o.WaitTimeout = 15 * time.Second
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	return nil
}

// PaperDimensions returns width and height in inches, swapped if landscape.
func (o *Options) PaperDimensions() (w, h float64) {
	dims := paperSizes[o.Format]
	w, h = dims[0], dims[1]
	if o.Landscape {
		w, h = h, w
	}
	return
}

// ParseMargin parses a "10mm" / "0.5in" / "20px" string into inches.
// Empty input returns 0 (no margin).
func ParseMargin(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	unit := ""
	for _, suffix := range []string{"mm", "cm", "in", "px"} {
		if strings.HasSuffix(s, suffix) {
			unit = suffix
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("margin %q is not a number", s)
	}
	switch unit {
	case "mm":
		return v / 25.4, nil
	case "cm":
		return v / 2.54, nil
	case "px":
		// CSS px at 96dpi → inches.
		return v / 96.0, nil
	case "in", "":
		return v, nil
	default:
		return 0, fmt.Errorf("unsupported margin unit %q", unit)
	}
}
