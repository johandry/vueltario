package domain

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// ValidateOccurrenceBounds enforces TIME-01 and TIME-02:
// - Unknown (nil) is distinct from 0 and allowed for either or both bounds.
// - start_ms >= 0
// - end_ms > start_ms when both known
// - bounds <= sourceDurationMs when known
func ValidateOccurrenceBounds(startMs, endMs, sourceDurationMs *int64) error {
	if startMs != nil && *startMs < 0 {
		return fmt.Errorf("%w: start_ms (%d) must be >= 0", ErrValidation, *startMs)
	}
	if endMs != nil && *endMs < 0 {
		return fmt.Errorf("%w: end_ms (%d) must be >= 0", ErrValidation, *endMs)
	}
	if startMs != nil && endMs != nil && *endMs <= *startMs {
		return fmt.Errorf("%w: end_ms (%d) must be greater than start_ms (%d)", ErrValidation, *endMs, *startMs)
	}
	if sourceDurationMs != nil && *sourceDurationMs > 0 {
		if startMs != nil && *startMs > *sourceDurationMs {
			return fmt.Errorf("%w: start_ms (%d) exceeds known source duration (%d)", ErrValidation, *startMs, *sourceDurationMs)
		}
		if endMs != nil && *endMs > *sourceDurationMs {
			return fmt.Errorf("%w: end_ms (%d) exceeds known source duration (%d)", ErrValidation, *endMs, *sourceDurationMs)
		}
	}
	return nil
}

// ParseTimestampMs parses flexible user input ("MM:SS", "MM:SS.mmm", "HH:MM:SS.mmm", or decimal seconds "12.5")
// into milliseconds. Empty string returns (nil, nil) representing unknown time.
func ParseTimestampMs(raw string) (*int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ":")
	var totalSeconds float64
	switch len(parts) {
	case 1:
		sec, err := strconv.ParseFloat(parts[0], 64)
		if err != nil || sec < 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
			return nil, fmt.Errorf("%w: invalid timestamp %q", ErrValidation, raw)
		}
		totalSeconds = sec
	case 2:
		mins, err1 := strconv.Atoi(parts[0])
		sec, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil || mins < 0 || sec < 0 || sec >= 60 {
			return nil, fmt.Errorf("%w: invalid MM:SS timestamp %q", ErrValidation, raw)
		}
		totalSeconds = float64(mins*60) + sec
	case 3:
		hrs, err1 := strconv.Atoi(parts[0])
		mins, err2 := strconv.Atoi(parts[1])
		sec, err3 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil || err3 != nil || hrs < 0 || mins < 0 || mins >= 60 || sec < 0 || sec >= 60 {
			return nil, fmt.Errorf("%w: invalid HH:MM:SS timestamp %q", ErrValidation, raw)
		}
		totalSeconds = float64(hrs*3600+mins*60) + sec
	default:
		return nil, fmt.Errorf("%w: unsupported timestamp format %q", ErrValidation, raw)
	}

	ms := int64(math.Round(totalSeconds * 1000))
	return &ms, nil
}

// FormatTimestampMs formats milliseconds as MM:SS.mmm (or MM:SS if exact second when compact=true).
func FormatTimestampMs(ms *int64, compact bool) string {
	if ms == nil {
		return "unknown"
	}
	val := *ms
	if val < 0 {
		val = 0
	}
	totalSec := val / 1000
	remMs := val % 1000
	hrs := totalSec / 3600
	mins := (totalSec % 3600) / 60
	secs := totalSec % 60
	if hrs > 0 {
		if compact && remMs == 0 {
			return fmt.Sprintf("%02d:%02d:%02d", hrs, mins, secs)
		}
		return fmt.Sprintf("%02d:%02d:%02d.%03d", hrs, mins, secs, remMs)
	}
	if compact && remMs == 0 {
		return fmt.Sprintf("%02d:%02d", mins, secs)
	}
	return fmt.Sprintf("%02d:%02d.%03d", mins, secs, remMs)
}

// FoldAccents strips diacritical marks while preserving base letters (e.g., "Exhibición" -> "Exhibicion", "Muñeca" -> "Muneca").
func FoldAccents(input string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, input)
	if err != nil {
		return input
	}
	return out
}

// NormalizeSearchKey normalizes Unicode, folds accents, lowercases, and collapses non-alphanumeric separators into single spaces.
func NormalizeSearchKey(input string) string {
	folded := strings.ToLower(FoldAccents(strings.TrimSpace(input)))
	var b strings.Builder
	lastSpace := true
	for _, r := range folded {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// PortableMoveToken turns a canonical move name into PascalCase ASCII/Unicode letters/digits with accents folded,
// e.g., "Dile que no" -> "DileQueNo", "Enchufla doble" -> "EnchuflaDoble", "Setenta y complicación" -> "SetentaYComplicacion".
func PortableMoveToken(canonicalName, disambiguator string) string {
	clean := FoldAccents(strings.TrimSpace(canonicalName))
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range clean {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()

	var token strings.Builder
	for _, w := range words {
		runesSlice := []rune(strings.ToLower(w))
		if len(runesSlice) == 0 {
			continue
		}
		runesSlice[0] = unicode.ToUpper(runesSlice[0])
		token.WriteString(string(runesSlice))
	}

	if d := PortableDisambiguator(disambiguator); d != "" {
		token.WriteString(d)
	}
	return token.String()
}

func PortableDisambiguator(disambiguator string) string {
	clean := FoldAccents(strings.TrimSpace(disambiguator))
	var b strings.Builder
	capNext := true
	for _, r := range clean {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if capNext {
				b.WriteRune(unicode.ToUpper(r))
				capNext = false
			} else {
				b.WriteRune(r)
			}
		} else {
			capNext = true
		}
	}
	return b.String()
}

// SanitizeBrandPrefix cleans a configured brand prefix while preserving capitalization (default "CubanSocial").
func SanitizeBrandPrefix(prefix string) string {
	p := FoldAccents(strings.TrimSpace(strings.TrimPrefix(prefix, "#")))
	var b strings.Builder
	for _, r := range p {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "CubanSocial"
	}
	return b.String()
}

// BuildMoveTag constructs the explicit dash-separated display label (#CubanSocial-DileQueNo)
// and, ONLY when includeCompanion is explicitly true, the compatible underscore companion (#CubanSocial_DileQueNo).
func BuildMoveTag(brandPrefix, canonicalName, disambiguator string, includeCompanion bool) MoveTag {
	brand := SanitizeBrandPrefix(brandPrefix)
	token := PortableMoveToken(canonicalName, disambiguator)
	tag := MoveTag{
		CanonicalName: strings.TrimSpace(canonicalName),
		PortableToken: token,
		DisplayLabel:  fmt.Sprintf("#%s-%s", brand, token),
	}
	if includeCompanion && token != "" {
		tag.PlatformHashtag = fmt.Sprintf("#%s_%s", brand, token)
	}
	return tag
}

// DetectCombinationCycle verifies that setting combinationMoveID's children to proposedChildren
// introduces neither a self-reference nor a transitive cycle in existingAdjacency.
func DetectCombinationCycle(combinationMoveID string, proposedChildren []string, existingAdjacency map[string][]string) error {
	for _, child := range proposedChildren {
		if child == "" {
			return fmt.Errorf("%w: combination step child_move_id cannot be empty", ErrValidation)
		}
		if child == combinationMoveID {
			return fmt.Errorf("%w: combination %s cannot reference itself", ErrValidation, combinationMoveID)
		}
	}

	// DFS from each proposed child to check if combinationMoveID is reachable.
	visited := make(map[string]bool)
	var canReachTarget func(node string) bool
	canReachTarget = func(node string) bool {
		if node == combinationMoveID {
			return true
		}
		if visited[node] {
			return false
		}
		visited[node] = true
		var next []string
		if node == combinationMoveID {
			next = proposedChildren
		} else {
			next = existingAdjacency[node]
		}
		for _, child := range next {
			if canReachTarget(child) {
				return true
			}
		}
		return false
	}

	for _, child := range proposedChildren {
		if canReachTarget(child) {
			return fmt.Errorf("%w: cyclic combination detected involving move %s", ErrValidation, combinationMoveID)
		}
	}
	return nil
}
