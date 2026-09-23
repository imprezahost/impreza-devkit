package scanner

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

const GitHubAttribution = "GitHub Advisory Database (reviewed), CC-BY-4.0, https://github.com/github/advisory-database; normalized by Impreza via OSV.dev"
const GoAttribution = "Go Vulnerability Database, CC-BY-4.0, https://vuln.go.dev; normalized by Impreza via OSV.dev"

type OSVRecord struct {
	ID               string   `json:"id"`
	Modified         string   `json:"modified"`
	Withdrawn        string   `json:"withdrawn"`
	Aliases          []string `json:"aliases"`
	DatabaseSpecific struct {
		Reviewed bool   `json:"github_reviewed"`
		Severity string `json:"severity"`
		URL      string `json:"url"`
	} `json:"database_specific"`
	Affected []struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Versions []string `json:"versions"`
		Ranges   []struct {
			Type   string              `json:"type"`
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
}

var ghsaID = regexp.MustCompile(`^GHSA-[23456789cfghjmpqrvwx]{4}-[23456789cfghjmpqrvwx]{4}-[23456789cfghjmpqrvwx]{4}$`)
var goID = regexp.MustCompile(`^GO-[0-9]{4}-[0-9]{4,7}$`)

// NormalizeOSV accepts the two selected source identities only. OSV's export is
// the trusted transport; ID prefixes alone never qualify an unreviewed record.
// Unsupported ordering is reported, never silently converted to a safe verdict.
func NormalizeOSV(raw []byte, ecosystem string) ([]AdvisoryEntry, bool, error) {
	if len(raw) > 2<<20 {
		return nil, false, errors.New("OSV record exceeds limit")
	}
	var r OSVRecord
	if json.Unmarshal(raw, &r) != nil {
		return nil, false, errors.New("invalid OSV record")
	}
	if r.Withdrawn != "" {
		return nil, false, nil
	}
	source := ""
	if ghsaID.MatchString(r.ID) && r.DatabaseSpecific.Reviewed {
		source = "github-reviewed"
	}
	if ecosystem == "Go" && goID.MatchString(r.ID) && r.DatabaseSpecific.URL == "https://pkg.go.dev/vuln/"+r.ID {
		source = "go-vulndb"
	}
	if source == "" {
		return nil, false, nil
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Modified); err != nil {
		return nil, false, errors.New("invalid OSV timestamp")
	}
	if len(r.Affected) > 500 || len(r.Aliases) > 32 {
		return nil, false, errors.New("OSV collection exceeds limit")
	}
	aliases := make([]string, 0, len(r.Aliases))
	for _, a := range r.Aliases {
		// Some upstream aliases are URLs. They are not identifiers and must not
		// cross the finding boundary. The advisory and its ranges remain usable.
		if idRe.MatchString(a) {
			aliases = append(aliases, a)
		}
	}
	severity := strings.ToLower(r.DatabaseSpecific.Severity)
	if severity != "low" && severity != "moderate" && severity != "high" && severity != "critical" {
		severity = "unknown"
	}
	var entries []AdvisoryEntry
	incomplete := false
	for _, a := range r.Affected {
		if a.Package.Ecosystem != ecosystem {
			continue
		}
		e := AdvisoryEntry{Ecosystem: ecosystem, Name: a.Package.Name, ID: r.ID, Sev: severity, Source: source, Aliases: aliases}
		if !depNameRe.MatchString(e.Name) || len(a.Versions) > 5000 || len(a.Ranges) > 128 {
			incomplete = true
			continue
		}
		for _, v := range a.Versions {
			if versionRe.MatchString(v) {
				e.Versions = append(e.Versions, v)
			} else {
				incomplete = true
			}
		}
		for _, rr := range a.Ranges {
			if rr.Type != "SEMVER" && rr.Type != "ECOSYSTEM" {
				incomplete = true
				continue
			}
			if len(rr.Events) > 256 {
				incomplete = true
				continue
			}
			var intervals []Interval
			var current *Interval
			ok := true
			for _, event := range rr.Events {
				if len(event) != 1 {
					ok = false
					break
				}
				if value, exists := event["introduced"]; exists {
					if current != nil {
						ok = false
						break
					}
					current = &Interval{Introduced: value}
				} else {
					if current == nil {
						ok = false
						break
					}
					for kind, value := range event {
						switch kind {
						case "fixed":
							current.Fixed = value
						case "last_affected":
							current.LastAffected = value
						case "limit":
							current.Limit = value
						default:
							ok = false
						}
						if value == "" {
							ok = false
						}
					}
					intervals = append(intervals, *current)
					current = nil
				}
			}
			if current != nil {
				intervals = append(intervals, *current)
			}
			if len(intervals) == 0 {
				ok = false
			}
			for _, interval := range intervals {
				if !validInterval(ecosystem, interval) {
					ok = false
				}
			}
			if !ok {
				incomplete = true
				continue
			}
			e.Ranges = append(e.Ranges, intervals...)
		}
		if len(e.Ranges) > 128 {
			incomplete = true
			continue
		}
		if len(e.Ranges) == 0 && len(e.Versions) == 0 {
			incomplete = true
			continue
		}
		entries = append(entries, e)
	}
	return entries, incomplete, nil
}

func NewAdvisoryBase(entries []AdvisoryEntry, revision uint64, now time.Time, incomplete bool) *AdvisoryBase {
	// Stable order makes repeatable comparisons possible. Prefer Go's source for
	// a Go finding; alias suppression happens only after each range matches.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Ecosystem != b.Ecosystem {
			return a.Ecosystem < b.Ecosystem
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Source != b.Source {
			return a.Source == "go-vulndb"
		}
		return a.ID < b.ID
	})
	return &AdvisoryBase{Schema: 2, Revision: revision, GeneratedAt: now.UTC().Format(time.RFC3339), ExpiresAt: now.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339), Entries: entries, Incomplete: incomplete, Attribution: []string{GitHubAttribution, GoAttribution}}
}
