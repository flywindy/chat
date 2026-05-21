// Package displayfmt provides display-formatting helpers shared across services.
package displayfmt

import "strings"

// CombineWithFallback joins first and second with a space; returns the non-empty side or fallback when both are empty.
func CombineWithFallback(first, second, fallback string) string {
	first = strings.TrimSpace(first)
	second = strings.TrimSpace(second)
	switch {
	case first == "" && second == "":
		return fallback
	case first == "":
		return second
	case second == "":
		return first
	case first == second:
		return first
	default:
		return first + " " + second
	}
}
