package icu

import (
	"encoding/json"
	"strings"
	"sync"
)

// TerritoryInfo is the regional data exposed by Intl.Locale's information
// methods. Weekdays use ISO numbering: Monday is 1 and Sunday is 7.
type TerritoryInfo struct {
	Calendars   []string `json:"calendars"`
	HourCycles  []string `json:"hourCycles"`
	FirstDay    int      `json:"firstDay"`
	MinimumDays int      `json:"minimalDays"`
	Weekend     []int    `json:"weekend"`
	TimeZones   []string `json:"timeZones"`
}

type localeInfoTable struct {
	Likely           map[string]string        `json:"likely"`
	Territory        map[string]TerritoryInfo `json:"territory"`
	HourCycles       map[string][]string      `json:"hourCycles"`
	Direction        map[string]string        `json:"direction"`
	Collations       map[string][]string      `json:"collations"`
	NumberingSystems map[string]string        `json:"numberingSystems"`
}

var (
	localeInfoOnce sync.Once
	localeInfoData localeInfoTable
)

func loadLocaleInfo() {
	localeInfoOnce.Do(func() {
		text, err := inflate(localeInfoPacked)
		if err != nil || json.Unmarshal([]byte(text), &localeInfoData) != nil {
			localeInfoData = localeInfoTable{}
		}
	})
}

// AddLikelySubtags fills the missing language, script, and region according to
// CLDR. Explicit subtags are retained; "und" is replaced by the likely
// language.
func AddLikelySubtags(language, script, region string) (string, string, string) {
	loadLocaleInfo()
	keys := []string{
		joinLocaleParts(language, script, region),
		joinLocaleParts(language, script, ""),
		joinLocaleParts(language, "", region),
		language,
	}
	var likely string
	for _, key := range keys {
		if key == "" {
			continue
		}
		if likely = localeInfoData.Likely[key]; likely != "" {
			break
		}
	}
	if likely == "" {
		return language, script, region
	}
	parts := strings.Split(likely, "-")
	if len(parts) < 3 {
		parts = []string{"en", "Latn", "US"}
	}
	if language == "" || language == "und" {
		language = parts[0]
	}
	if script == "" {
		script = parts[1]
	}
	if region == "" {
		region = parts[2]
	}
	return language, script, region
}

func joinLocaleParts(language, script, region string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{language, script, region} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "-")
}

// TerritoryInfoFor returns regional locale preferences. Unknown territories
// use the CLDR world defaults.
func TerritoryInfoFor(region string) TerritoryInfo {
	loadLocaleInfo()
	if info, ok := localeInfoData.Territory[region]; ok {
		return info
	}
	return localeInfoData.Territory["001"]
}

// WeekInfoForLocale returns the regional first day of the week and the
// minimum number of days which make the first week of a year. The locale data
// in older generated tables predates MinimumDays, so retain the CLDR rule for
// those tables until they are regenerated.
func WeekInfoForLocale(tag string) (firstDay, minimumDays int) {
	base := strings.Split(tag, "-u-")[0]
	parts := strings.Split(base, "-")
	language, script, region := "und", "", ""
	if len(parts) > 0 && parts[0] != "" {
		language = strings.ToLower(parts[0])
	}
	for _, part := range parts[1:] {
		switch {
		case len(part) == 4:
			script = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
		case len(part) == 2 || (len(part) == 3 && part[0] >= '0' && part[0] <= '9'):
			region = strings.ToUpper(part)
		}
	}
	_, _, region = AddLikelySubtags(language, script, region)
	info := TerritoryInfoFor(region)
	firstDay, minimumDays = info.FirstDay, info.MinimumDays
	if firstDay == 0 {
		firstDay = 1
	}
	if minimumDays == 0 {
		minimumDays = 1
		if minimumDaysFour[region] {
			minimumDays = 4
		}
	}
	return firstDay, minimumDays
}

var minimumDaysFour = map[string]bool{
	"AD": true, "AN": true, "AT": true, "AX": true, "BE": true,
	"BG": true, "CH": true, "CZ": true, "DE": true, "DK": true,
	"EE": true, "ES": true, "FI": true, "FJ": true, "FO": true,
	"FR": true, "GB": true, "GF": true, "GG": true, "GI": true,
	"GP": true, "GR": true, "IE": true, "IM": true, "IS": true,
	"IT": true, "JE": true, "LI": true, "LT": true, "LU": true,
	"MC": true, "MQ": true, "NL": true, "NO": true, "PL": true,
	"PT": true, "RE": true, "RU": true, "SE": true, "SJ": true,
	"SK": true, "SM": true, "VA": true,
}

// TerritoryInfoExact returns data only when the requested territory exists.
// It is used by getTimeZones, where an unknown explicit region has no zones
// rather than inheriting the world-region fallback used by other methods.
func TerritoryInfoExact(region string) (TerritoryInfo, bool) {
	loadLocaleInfo()
	info, ok := localeInfoData.Territory[region]
	return info, ok
}

// HasTerritoryInfo reports whether CLDR has preferences for a territory. It
// distinguishes an unknown rg override from a real territory using defaults.
func HasTerritoryInfo(region string) bool {
	loadLocaleInfo()
	_, ok := localeInfoData.Territory[region]
	return ok
}

// LocaleHourCycles returns the language-specific clock preference where CLDR
// has one, then the territory preference.
func LocaleHourCycles(language, region string) []string {
	loadLocaleInfo()
	if cycles := localeInfoData.HourCycles[language+"-"+region]; len(cycles) > 0 {
		return cycles
	}
	return TerritoryInfoFor(region).HourCycles
}

// ScriptDirection reports whether a script is written left-to-right or
// right-to-left. Unknown scripts follow the root left-to-right default.
func ScriptDirection(script string) string {
	loadLocaleInfo()
	if direction := localeInfoData.Direction[script]; direction != "" {
		return direction
	}
	return "ltr"
}

// LocaleCollations returns the collation choices attached to a language and
// explicit script. An unsupported explicit script uses the root choices
// instead of falling back to the language's usual script.
func LocaleCollations(language, script string) []string {
	loadLocaleInfo()
	key := language
	if script != "" {
		key += "-" + script
	}
	if values := localeInfoData.Collations[key]; len(values) > 0 {
		return values
	}
	return []string{"emoji", "eor"}
}

// LocaleNumberingSystem returns the single numbering system exposed by ICU's
// Intl.Locale data. Explicit scripts take precedence over regional choices;
// Arabic in ar and ur is the exception whose native script still admits the
// region-specific choice.
func LocaleNumberingSystem(language, script, region string) string {
	loadLocaleInfo()
	if script != "" {
		if value := localeInfoData.NumberingSystems[language+"-"+script]; value != "" {
			return value
		}
		if script != "Arab" || language != "ar" && language != "ur" {
			return "latn"
		}
	}
	if region != "" {
		if value, ok := localeInfoData.NumberingSystems[language+"-"+region]; ok {
			return value
		}
	}
	if script == "" {
		if value := localeInfoData.NumberingSystems[language]; value != "" {
			return value
		}
	}
	return "latn"
}
