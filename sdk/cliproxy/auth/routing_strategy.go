package auth

import "strings"

const (
	// RoutingStrategyRoundRobin rotates across ready credentials.
	RoutingStrategyRoundRobin = "round-robin"
	// RoutingStrategyWeightedRoundRobin rotates credentials according to their configured weights.
	RoutingStrategyWeightedRoundRobin = "weighted-round-robin"
	// RoutingStrategyFillFirst burns the first ready credential before moving on.
	RoutingStrategyFillFirst = "fill-first"
)

// NormalizeRoutingStrategy canonicalizes supported routing strategy names and aliases.
func NormalizeRoutingStrategy(strategy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", RoutingStrategyRoundRobin, "roundrobin", "rr":
		return RoutingStrategyRoundRobin, true
	case RoutingStrategyWeightedRoundRobin, "weightedroundrobin", "wrr":
		return RoutingStrategyWeightedRoundRobin, true
	case RoutingStrategyFillFirst, "fillfirst", "ff":
		return RoutingStrategyFillFirst, true
	default:
		return "", false
	}
}

// SelectorForRoutingStrategy returns the built-in selector for the supplied strategy.
// Unknown values fall back to round-robin so startup and reload behavior stay safe.
func SelectorForRoutingStrategy(strategy string) Selector {
	normalized, ok := NormalizeRoutingStrategy(strategy)
	if !ok {
		normalized = RoutingStrategyRoundRobin
	}
	switch normalized {
	case RoutingStrategyWeightedRoundRobin:
		return &WeightedRoundRobinSelector{}
	case RoutingStrategyFillFirst:
		return &FillFirstSelector{}
	default:
		return &RoundRobinSelector{}
	}
}
