// Package editdiff implements edit-diff semantics (core-spec §4.5, matching
// the reference edit-diff module): unique-match replacement with BOM/line-
// ending handling, fuzzy matching fallback, and overlap detection.
package editdiff

import (
	"errors"
	"fmt"
	"strings"
)

// Edit is one targeted replacement.
type Edit struct {
	OldText string
	NewText string
}

// Result of applying edits.
type Result struct {
	BaseContent string // normalized original
	NewContent  string // normalized result
	Changed     bool
}

// ApplyError is a user-facing edit error.
type ApplyError struct {
	Message string
}

func (e *ApplyError) Error() string { return e.Message }

var errNoChange = errors.New("no change")

// Errors returned by Apply (sentinel-wrapped ApplyErrors).
func NotFoundError(path string, editIndex, totalEdits int) error {
	if totalEdits == 1 {
		return &ApplyError{Message: fmt.Sprintf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)}
	}
	return &ApplyError{Message: fmt.Sprintf("Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.", editIndex, path)}
}

func DuplicateError(path string, editIndex, totalEdits, occurrences int) error {
	if totalEdits == 1 {
		return &ApplyError{Message: fmt.Sprintf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occurrences, path)}
	}
	return &ApplyError{Message: fmt.Sprintf("Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.", occurrences, editIndex, path)}
}

func NoChangeError(path string, totalEdits int) error {
	if totalEdits == 1 {
		return &ApplyError{Message: fmt.Sprintf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)}
	}
	return &ApplyError{Message: fmt.Sprintf("No changes made to %s. The replacements produced identical content.", path)}
}

func EmptyOldTextError(path string, editIndex, totalEdits int) error {
	if totalEdits == 1 {
		return &ApplyError{Message: fmt.Sprintf("oldText must not be empty in %s.", path)}
	}
	return &ApplyError{Message: fmt.Sprintf("edits[%d].oldText must not be empty in %s.", editIndex, path)}
}

// --- line endings / BOM ----------------------------------------------------

// StripBOM removes a UTF-8 BOM, returning it separately.
func StripBOM(content string) (bom, text string) {
	if strings.HasPrefix(content, "\uFEFF") {
		return "\uFEFF", content[3:]
	}
	return "", content
}

// DetectLineEnding returns "\r\n" or "\n" (default "\n").
func DetectLineEnding(content string) string {
	idx := strings.Index(content, "\n")
	if idx > 0 && content[idx-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// NormalizeToLF converts CRLF/CR to LF.
func NormalizeToLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// RestoreLineEndings converts LF back to the original ending.
func RestoreLineEndings(s, ending string) string {
	if ending == "" || ending == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", ending)
}

// NormalizeForFuzzyMatch normalizes whitespace and punctuation for fuzzy
// matching (NFKC is not applied in Go; the whitespace/punctuation
// normalization is the important part for LLM-generated edits).
func NormalizeForFuzzyMatch(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	text = strings.Join(lines, "\n")
	repl := strings.NewReplacer(
		"\u2018", "'", "\u2019", "'", "\u201A", "'", "\u201B", "'",
		"\u201C", "\"", "\u201D", "\"", "\u201E", "\"", "\u201F", "\"",
		"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-",
		"\u2014", "-", "\u2015", "-", "\u2212", "-",
		"\u00A0", " ", "\u2002", " ", "\u2003", " ", "\u2004", " ", "\u2005", " ",
		"\u2006", " ", "\u2007", " ", "\u2008", " ", "\u2009", " ", "\u200A", " ",
		"\u202F", " ", "\u205F", " ", "\u3000", " ",
	)
	return repl.Replace(text)
}

// --- matching ---------------------------------------------------------------

type matchResult struct {
	found                 bool
	index                 int
	matchLength           int
	usedFuzzyMatch        bool
	contentForReplacement string
}

func fuzzyFind(content, oldText string) matchResult {
	exact := strings.Index(content, oldText)
	if exact != -1 {
		return matchResult{found: true, index: exact, matchLength: len(oldText)}
	}
	fuzzyContent := NormalizeForFuzzyMatch(content)
	fuzzyOld := NormalizeForFuzzyMatch(oldText)
	fuzzyIdx := strings.Index(fuzzyContent, fuzzyOld)
	if fuzzyIdx == -1 {
		return matchResult{found: false}
	}
	return matchResult{found: true, index: fuzzyIdx, matchLength: len(fuzzyOld), usedFuzzyMatch: true, contentForReplacement: fuzzyContent}
}

func countOccurrences(content, needle string) int {
	if needle == "" {
		return 0
	}
	n := 0
	idx := 0
	for {
		i := strings.Index(content[idx:], needle)
		if i == -1 {
			break
		}
		n++
		idx += i + len(needle)
	}
	return n
}

type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

// Apply applies edits to normalized content.
func Apply(normalizedContent string, edits []Edit, path string) (Result, error) {
	normalizedEdits := make([]Edit, len(edits))
	for i, e := range edits {
		normalizedEdits[i] = Edit{OldText: NormalizeToLF(e.OldText), NewText: NormalizeToLF(e.NewText)}
		if normalizedEdits[i].OldText == "" {
			return Result{}, EmptyOldTextError(path, i, len(edits))
		}
	}

	initial := make([]matchResult, len(normalizedEdits))
	usedFuzzy := false
	for i, e := range normalizedEdits {
		m := fuzzyFind(normalizedContent, e.OldText)
		initial[i] = m
		if m.usedFuzzyMatch {
			usedFuzzy = true
		}
	}
	replacementBase := normalizedContent
	if usedFuzzy {
		replacementBase = NormalizeForFuzzyMatch(normalizedContent)
	}

	var matched []matchedEdit
	for i, e := range normalizedEdits {
		m := fuzzyFind(replacementBase, e.OldText)
		if !m.found {
			return Result{}, NotFoundError(path, i, len(edits))
		}
		occ := countOccurrences(replacementBase, e.OldText)
		if occ > 1 {
			return Result{}, DuplicateError(path, i, len(edits), occ)
		}
		matched = append(matched, matchedEdit{
			editIndex: i, matchIndex: m.index, matchLength: m.matchLength, newText: e.NewText,
		})
	}
	sortMatched(matched)
	for i := 1; i < len(matched); i++ {
		prev, cur := matched[i-1], matched[i]
		if prev.matchIndex+prev.matchLength > cur.matchIndex {
			return Result{}, &ApplyError{Message: fmt.Sprintf("edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.", prev.editIndex, cur.editIndex, path)}
		}
	}

	base := normalizedContent
	var newContent string
	if usedFuzzy {
		newContent = applyReplacementsPreserving(normalizedContent, replacementBase, matched)
	} else {
		newContent = applyReplacements(replacementBase, matched)
	}
	if base == newContent {
		return Result{}, NoChangeError(path, len(edits))
	}
	return Result{BaseContent: base, NewContent: newContent, Changed: true}, nil
}

func sortMatched(ms []matchedEdit) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].matchIndex < ms[j-1].matchIndex; j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

func applyReplacements(content string, matched []matchedEdit) string {
	var sb strings.Builder
	pos := 0
	for _, m := range matched {
		sb.WriteString(content[pos:m.matchIndex])
		sb.WriteString(m.newText)
		pos = m.matchIndex + m.matchLength
	}
	sb.WriteString(content[pos:])
	return sb.String()
}

// applyReplacementsPreserving replaces in fuzzy space but preserves the
// original (non-normalized) text for unchanged regions. Because offsets were
// computed in fuzzy space, we map back line-by-line when the content was
// normalized only by trailing-whitespace trimming.
func applyReplacementsPreserving(original, fuzzy string, matched []matchedEdit) string {
	origLines := strings.Split(original, "\n")
	fuzzyLines := strings.Split(fuzzy, "\n")
	// Both should have the same line count after trimming.
	var out []string
	fuzzyPos := 0 // char offset in fuzzy space
	editPtr := 0
	for lineIdx := 0; lineIdx < len(origLines); lineIdx++ {
		origLine := origLines[lineIdx]
		fuzzyLine := fuzzyLines[lineIdx]
		lineOut := origLine
		lineFuzzyStart := fuzzyPos
		_ = lineFuzzyStart
		// Build the output line by applying edits that fall within this line.
		var sb strings.Builder
		col := 0 // offset within the fuzzy line
		consumed := 0
		for editPtr < len(matched) {
			m := matched[editPtr]
			// Edits are sorted; find ones intersecting this line's fuzzy span.
			if m.matchIndex < fuzzyPos+len(fuzzyLine) {
				// Intersects this line.
				localStart := m.matchIndex - fuzzyPos
				if localStart < 0 {
					localStart = 0
				}
				if localStart > len(fuzzyLine) {
					break
				}
				sb.WriteString(fuzzyLine[col:localStart])
				sb.WriteString(m.newText)
				consumed = localStart + m.matchLength
				col = consumed
				editPtr++
				continue
			}
			break
		}
		if col > 0 {
			sb.WriteString(fuzzyLine[col:])
			lineOut = sb.String()
		} else {
			lineOut = origLine
		}
		out = append(out, lineOut)
		fuzzyPos += len(fuzzyLine) + 1
	}
	return strings.Join(out, "\n")
}

// GenerateDiff returns a simple unified-ish diff for details.
func GenerateDiff(base, newContent string) (string, int) {
	baseLines := strings.Split(base, "\n")
	newLines := strings.Split(newContent, "\n")
	var sb strings.Builder
	firstChanged := -1
	max := len(baseLines)
	if len(newLines) > max {
		max = len(newLines)
	}
	for i := 0; i < max; i++ {
		var bl, nl string
		if i < len(baseLines) {
			bl = baseLines[i]
		}
		if i < len(newLines) {
			nl = newLines[i]
		}
		if bl != nl {
			if firstChanged == -1 {
				firstChanged = i + 1
			}
			if bl != "" || i < len(baseLines) {
				sb.WriteString("-" + bl + "\n")
			}
			if nl != "" || i < len(newLines) {
				sb.WriteString("+" + nl + "\n")
			}
		}
	}
	return sb.String(), firstChanged
}
