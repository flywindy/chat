package main

import (
	"math"
	"time"
)

const (
	envelopeBaseline = 0.4
	envelopeSwing    = 0.6
	envelopeSigma    = 0.12 // fraction of hold; controls peak width
)

// rateMultiplier returns the diurnal envelope value at `elapsed` into a
// hold window of length `hold`. Range is [envelopeBaseline, envelopeBaseline+envelopeSwing].
// The shape is the max of two Gaussians centred at 1/3 and 2/3 of hold,
// approximating a workday with morning and afternoon peaks.
//
// Returns 1.0 when hold is zero (degenerate case used by some tests).
func rateMultiplier(elapsed, hold time.Duration) float64 {
	if hold <= 0 {
		return 1.0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > hold {
		elapsed = hold
	}
	x := float64(elapsed) / float64(hold)
	g := func(centre float64) float64 {
		d := (x - centre) / envelopeSigma
		return math.Exp(-0.5 * d * d)
	}
	peak := math.Max(g(1.0/3.0), g(2.0/3.0))
	return envelopeBaseline + envelopeSwing*peak
}
