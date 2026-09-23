package scanner

import (
	"regexp"
	"strings"

	"deps.dev/util/semver"
)

// Composer accepts suffixes without a separator (e.g. 4.3alpha1). Adapt only
// that documented notation; ordering still belongs to the ecosystem library.
var composerSuffix = regexp.MustCompile(`(?i)^(v?[0-9]+(?:\.[0-9]+){0,3})(alpha|beta|rc|a|b)([0-9]*)$`)

// Intervals are normalized OSV events: lower inclusive, fixed/limit exclusive,
// last_affected inclusive. An empty upper bound means no known fixed version.
type Interval struct {
	Introduced   string `json:"introduced"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}

func parseVersion(ecosystem, text string) (*semver.Version, error) {
	sys := semver.NPM
	switch ecosystem {
	case "Go":
		sys = semver.Go
		text = "v" + strings.TrimPrefix(text, "v")
	case "Packagist":
		sys = semver.Composer
		text = composerSuffix.ReplaceAllString(text, "${1}-${2}${3}")
	}
	return sys.Parse(text)
}

func validVersion(ecosystem, value string) bool {
	if !versionRe.MatchString(value) {
		return false
	}
	v, err := parseVersion(ecosystem, value)
	return err == nil && !v.IsWildcard()
}

func validInterval(ecosystem string, r Interval) bool {
	if r.Introduced != "0" && !validVersion(ecosystem, r.Introduced) {
		return false
	}
	ends := 0
	for _, bound := range []string{r.Fixed, r.LastAffected, r.Limit} {
		if bound == "" {
			continue
		}
		ends++
		if !validVersion(ecosystem, bound) {
			return false
		}
		if r.Introduced != "0" {
			lo, _ := parseVersion(ecosystem, r.Introduced)
			hi, _ := parseVersion(ecosystem, bound)
			if lo.Compare(hi) > 0 || (lo.Compare(hi) == 0 && r.LastAffected == "") {
				return false
			}
		}
	}
	return ends <= 1
}

func affected(e AdvisoryEntry, version string) (bool, bool) {
	// Preserve explicitly enumerated versions, including versions for which the
	// ecosystem has no supported ordering (e.g. a Composer development branch).
	for _, exact := range append([]string{e.Version}, e.Versions...) {
		if exact != "" && strings.TrimPrefix(exact, "v") == strings.TrimPrefix(version, "v") {
			return true, true
		}
	}
	if len(e.Ranges) == 0 {
		return false, true
	}
	v, err := parseVersion(e.Ecosystem, version)
	if err != nil || v.IsWildcard() {
		return false, false
	}
	for _, r := range e.Ranges {
		if r.Introduced != "0" {
			lo, _ := parseVersion(e.Ecosystem, r.Introduced)
			if v.Compare(lo) < 0 {
				continue
			}
		}
		upper := r.Fixed
		if r.Limit != "" {
			upper = r.Limit
		}
		if r.LastAffected != "" {
			upper = r.LastAffected
		}
		if upper == "" {
			return true, true
		}
		hi, _ := parseVersion(e.Ecosystem, upper)
		cmp := v.Compare(hi)
		if cmp < 0 || (cmp == 0 && r.LastAffected != "") {
			return true, true
		}
	}
	return false, true
}
