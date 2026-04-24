package agent

import "strings"

// HasAuthKeyword is a heuristic scan for auth / rate-limit / model-access
// trouble in an output or stderr text. Errs toward false-positives — a
// misclassified auth message is still actionable; missing one leaves the
// user without guidance. Exported so drivers can reuse it when
// classifying their output.
func HasAuthKeyword(s string) bool {
	keywords := []string{
		"api key",
		"api_key",
		"apikey",
		"unauthorized",
		"authentication",
		"auth failed",
		"invalid key",
		"rate limit",
		"rate_limit",
		"ratelimit",
		"quota",
		"throttl",
		"401",
		"429",
		"access denied",
		"accessdenied",
		"not enabled in region",
		"model access",
	}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
