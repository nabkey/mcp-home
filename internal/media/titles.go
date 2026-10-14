package media

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// foldTable maps the accented letters that show up in real lookup results
// (Shōgun, Pokémon, Amélie, Léon) to ASCII. A table, not Unicode
// normalisation, to avoid a dependency for a handful of letters.
var foldTable = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a",
	'ç': "c", 'č': "c",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ī': "i",
	'ñ': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ū': "u",
	'ý': "y", 'ÿ': "y",
	'š': "s", 'ž': "z", 'ß': "ss", 'æ': "ae", 'œ': "oe",
	'&': " and ",
}

// parenSuffix matches the "(US)", "(2003)", "(CA) (2012)" disambiguators
// TheTVDB appends to titles.
var parenSuffix = regexp.MustCompile(`(\s*\([^)]*\))+\s*$`)

// normalizeTitle reduces a title to what a person would say: case, accents,
// punctuation and trailing parentheticals removed.
func normalizeTitle(s string) string {
	s = parenSuffix.ReplaceAllString(strings.ToLower(s), "")
	var b strings.Builder
	space := false
	for _, r := range s {
		if f, ok := foldTable[r]; ok {
			b.WriteString(f)
			space = strings.HasSuffix(f, " ")
			continue
		}
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// titleMatches reports whether the title a caller gave names the record.
func titleMatches(given string, candidates ...string) bool {
	g := normalizeTitle(given)
	if g == "" {
		return false
	}
	for _, c := range candidates {
		if c != "" && normalizeTitle(c) == g {
			return true
		}
	}
	return false
}

// titleGroup is a set of results a person would call by the same name.
type titleGroup struct {
	Title   string   `json:"title"`
	Choices []string `json:"choices"`
}

// ambiguousGroups finds results sharing a normalised title. labels[i] is
// how result i should be named in the group (e.g. "1994 tmdb:8587").
func ambiguousGroups(titles, labels []string) []titleGroup {
	idx := map[string][]int{}
	var order []string
	for i, t := range titles {
		n := normalizeTitle(t)
		if _, seen := idx[n]; !seen {
			order = append(order, n)
		}
		idx[n] = append(idx[n], i)
	}
	var groups []titleGroup
	for _, n := range order {
		members := idx[n]
		if len(members) < 2 {
			continue
		}
		g := titleGroup{Title: titles[members[0]]}
		for _, i := range members {
			g.Choices = append(g.Choices, labels[i])
		}
		sort.Strings(g.Choices)
		groups = append(groups, g)
	}
	return groups
}

// truncate shortens s to at most n bytes on a word boundary.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], ' ')
	if cut < n/2 {
		cut = n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	return s[:cut] + "..."
}
