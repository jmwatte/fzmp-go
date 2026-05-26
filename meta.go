package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// layouts holds the active layout list, populated from config at startup.
var layouts []layoutConfig

// trackNum parses a field like "3", "03", or "3/12" into an integer.
// Returns 0 if the field is absent or unparseable.
func trackNum(t track, col string) int {
	v := t.fields[col]
	if v == "" {
		return 0
	}
	// Handle "N/Total" format
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

type metaItem struct {
	display string
	key     string
	sortKey string
	search  string // lowercased searchable text cache
	docs    []string
}

type metaLoadedMsg struct {
	tracks  []track
	headers []string // non-path column names
	errMsg  string
}

type metaModel struct {
	csvPath      string
	tracks       []track
	headers      []string // columns available in the CSV
	loaded       bool
	loadErr      string
	tracksByPath map[string]*track // index for O(1) field lookup in applyFilter

	layoutIdx int
	layout    []string // current drill-down column sequence

	stage        int      // 0..len(layout); len(layout) == track list
	selections   []string // one entry per completed stage
	stageCursors map[int]int
	stageFilters map[int]string // saved filter string per stage

	allItems []metaItem
	items    []metaItem
	cursor   int
	selected map[string]bool // keyed by track path; only meaningful at leaf stage

	filter       string
	filterCursor int
	filterMode   bool
	helpMode     bool
	helpScroll   int

	status               string
	isErr                bool
	height               int
	width                int
	splitGenres          bool
	splitGenreDelimiters []string

	artContent string
	artPath    string // path last submitted for rendering (stale-result guard)
}

func isGenreColumn(col string) bool {
	return strings.EqualFold(strings.TrimSpace(col), "Genre")
}

func normalizeSplitDelimiters(delims []string) []string {
	if len(delims) == 0 {
		return []string{";"}
	}
	seen := make(map[string]bool, len(delims))
	out := make([]string, 0, len(delims))
	for _, d := range delims {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return []string{";"}
	}
	return out
}

func splitGenreValue(raw string, delims []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{"Unknown"}
	}
	parts := []string{raw}
	for _, delim := range normalizeSplitDelimiters(delims) {
		next := make([]string, 0, len(parts))
		for _, part := range parts {
			next = append(next, strings.Split(part, delim)...)
		}
		parts = next
	}
	if len(parts) == 1 {
		return []string{raw}
	}
	seen := make(map[string]bool, len(parts))
	vals := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return []string{raw}
	}
	return vals
}

func (m *metaModel) facetValues(t track, col string) []string {
	v := fieldVal(t, col)
	if m.splitGenres && isGenreColumn(col) {
		return splitGenreValue(v, m.splitGenreDelimiters)
	}
	return []string{v}
}

func (m *metaModel) trackMatchesFacet(t track, col, want string) bool {
	for _, v := range m.facetValues(t, col) {
		if v == want {
			return true
		}
	}
	return false
}

var filterWordRE = regexp.MustCompile(`(?:[A-Za-z]+:)?"[^"]+"|\S+`)

func isASCIIAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isShortNumericToken(s string) bool {
	if len(s) == 0 || len(s) > 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func containsTokenBoundary(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for from := 0; ; {
		i := strings.Index(haystack[from:], needle)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(needle)
		leftOK := start == 0 || !isASCIIAlnum(haystack[start-1])
		rightOK := end == len(haystack) || !isASCIIAlnum(haystack[end])
		if leftOK && rightOK {
			return true
		}
		from = start + 1
		if from >= len(haystack) {
			return false
		}
	}
}

func tokenMatchesText(text string, tok filterToken) bool {
	if tok.value == "" {
		return true
	}
	if tok.exact {
		return strings.Contains(text, tok.value)
	}
	if isShortNumericToken(tok.value) {
		return containsTokenBoundary(text, tok.value)
	}
	return strings.Contains(text, tok.value)
}

func compileRegexFilter(q string) (*regexp.Regexp, bool, error) {
	t := strings.TrimSpace(q)
	if !strings.HasPrefix(strings.ToLower(t), "re:") {
		return nil, false, nil
	}
	pattern := strings.TrimSpace(t[3:])
	if pattern == "" {
		return nil, true, nil
	}
	rx, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, true, err
	}
	return rx, true, nil
}

func (m *metaModel) itemMatchesRegex(item metaItem, rx *regexp.Regexp) bool {
	if rx == nil {
		return true
	}
	if m.stage == len(m.layout) {
		return rx.MatchString(item.search)
	}
	for _, d := range item.docs {
		if rx.MatchString(d) {
			return true
		}
	}
	return rx.MatchString(item.search)
}

func activeFilterHints(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}

	_, regexMode, _ := compileRegexFilter(q)
	if regexMode {
		return "  [mode: regex]"
	}

	tokens := parseFilterTokens(q)
	hasPhrase := false
	hasShortNumeric := false
	for _, tok := range tokens {
		if tok.exact {
			hasPhrase = true
		}
		if len(tok.cols) == 0 && isShortNumericToken(tok.value) {
			hasShortNumeric = true
		}
	}

	var parts []string
	if hasPhrase {
		parts = append(parts, "phrase")
	}
	if hasShortNumeric {
		parts = append(parts, "num-boundary")
	}
	if len(parts) == 0 {
		return ""
	}
	return "  [mode: " + strings.Join(parts, ", ") + "]"
}

// artRefreshCmd computes the art path for the currently visible track (leaf
// only) and dispatches the appropriate command for the current art mode.
// Returns nil when art is off or already up-to-date.
func (m *metaModel) artRefreshCmd() tea.Cmd {
	if artMode == 0 {
		return nil
	}
	isLeaf := m.stage == len(m.layout)

	if isLeaf {
		// Leaf: derive album dir from the track path and use the same logic as folder view.
		var dir string
		if len(m.items) > 0 && m.cursor < len(m.items) {
			dir = filepath.Dir(m.items[m.cursor].key)
		}
		switch artMode {
		case 1:
			key, cmd := artChafaForDir(dir)
			if key != m.artPath && isTransientMosaicStatus(m.status, m.isErr) {
				m.status = ""
				m.isErr = false
			}
			if key == m.artPath {
				return nil
			}
			m.artPath = key
			if cmd == nil {
				return nil
			}
			return cmd
		case 2:
			key, cmd := openArtViewerForDir(dir)
			if key != m.artPath && isTransientMosaicStatus(m.status, m.isErr) {
				m.status = ""
				m.isErr = false
			}
			if key == m.artPath {
				return nil
			}
			m.artPath = key
			if cmd == nil {
				return nil
			}
			return cmd
		}
		return nil
	}

	// Non-leaf: collect one cover per unique album dir that matches the hovered facet value.
	covers := m.hoveredItemCovers()
	if len(covers) == 0 {
		if noArtPath == "" {
			return nil
		}
		covers = []string{noArtPath}
	}
	sort.Strings(covers)
	key := strings.Join(covers, "|")
	if key != m.artPath && isTransientMosaicStatus(m.status, m.isErr) {
		m.status = ""
		m.isErr = false
	}
	if key == m.artPath {
		return nil
	}
	m.artPath = key

	switch artMode {
	case 1:
		if len(covers) == 1 {
			return renderArtCmd(covers[0])
		}
		return renderArtGridCmd(covers)
	case 2:
		if len(covers) == 1 {
			return openArtViewer(covers[0])
		}
		return openArtViewerMosaic(covers, mosaicCachePath(covers))
	}
	return nil
}

// hoveredItemCovers returns one cover-image path per unique album directory
// that contains tracks matching the current drill-down path plus the hovered
// facet value at the current stage.  All stat calls are done here so the
// caller can pass the result directly to a render cmd.
func (m *metaModel) hoveredItemCovers() []string {
	if len(m.items) == 0 || m.cursor >= len(m.items) {
		return nil
	}
	hoveredVal := m.items[m.cursor].key
	col := m.layout[m.stage]

	seen := make(map[string]bool)
	var covers []string
	for _, t := range m.tracks {
		if !m.trackMatchesSelections(t) {
			continue
		}
		if !m.trackMatchesFacet(t, col, hoveredVal) {
			continue
		}
		dir := filepath.Dir(t.path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if p := findArtInDir(dir); p != "" {
			covers = append(covers, p)
		}
	}
	return covers
}

func initialMetaModel(cfg appConfig) metaModel {
	// Populate the global layout list from config.
	// If config has none, fall back to built-in defaults.
	if len(cfg.Meta.Layouts) > 0 {
		layouts = cfg.Meta.Layouts
	} else {
		layouts = []layoutConfig{
			{Name: "Genre › Artist › Album", Levels: []string{"Genre", "Album Artist", "Album"}},
			{Name: "Artist › Album", Levels: []string{"Album Artist", "Album"}},
			{Name: "Year › Artist › Album", Levels: []string{"Year", "Album Artist", "Album"}},
		}
	}
	csvP := cfg.Meta.CSVPath
	if csvP == "" {
		// Default: %APPDATA%\fzmp\metadata-cache.csv (same as PS fzmp)
		appdata := os.Getenv("APPDATA")
		csvP = filepath.Join(appdata, "fzmp", "metadata-cache.csv")
	}
	return metaModel{
		csvPath:              csvP,
		layout:               layouts[0].Levels,
		splitGenres:          cfg.Meta.SplitGenres,
		splitGenreDelimiters: normalizeSplitDelimiters(cfg.Meta.SplitGenreDelimiters),
		stageCursors:         make(map[int]int),
		stageFilters:         make(map[int]string), selected: make(map[string]bool), height: 24,
	}
}

func (m metaModel) Init() tea.Cmd { return nil }

func loadMetaCmd(csvPath string) tea.Cmd {
	return func() tea.Msg {
		tracks, headers, err := loadCSV(csvPath)
		if err != nil {
			return metaLoadedMsg{errMsg: err.Error()}
		}
		return metaLoadedMsg{tracks: tracks, headers: headers}
	}
}

// fieldVal returns the track field value for a column, falling back to "Unknown".
func fieldVal(t track, col string) string {
	v := t.fields[col]
	if v == "" {
		return "Unknown"
	}
	return v
}

// trackMatchesSelections returns true if the track satisfies all current drill-down selections.
func (m *metaModel) trackMatchesSelections(t track) bool {
	for i, col := range m.layout[:m.stage] {
		if !m.trackMatchesFacet(t, col, m.selections[i]) {
			return false
		}
	}
	return true
}

func (m *metaModel) computeAllItems() {
	if m.stage == len(m.layout) {
		// Leaf: collect matching tracks
		var result []track
		for _, t := range m.tracks {
			if m.trackMatchesSelections(t) {
				result = append(result, t)
			}
		}
		sort.Slice(result, func(i, j int) bool {
			di, dj := trackNum(result[i], "Discnumber"), trackNum(result[j], "Discnumber")
			if di != dj {
				return di < dj
			}
			ti, tj := trackNum(result[i], "Tracknumber"), trackNum(result[j], "Tracknumber")
			if ti != tj {
				return ti < tj
			}
			return strings.ToLower(result[i].fields["Title"]) < strings.ToLower(result[j].fields["Title"])
		})
		// Determine which fields are already locked in by the current selections
		// so we don't repeat them in the display label.
		selectedCols := make(map[string]bool, len(m.selections))
		for _, col := range m.layout[:m.stage] {
			selectedCols[col] = true
		}

		items := make([]metaItem, 0, len(result))
		for _, t := range result {
			title := t.fields["Title"]
			if title == "" {
				title = filepath.Base(t.path)
			}
			display := title
			// Append artist unless we already drilled in via an artist-type column.
			if !selectedCols["Album Artist"] && !selectedCols["Artist"] {
				artist := t.fields["Album Artist"]
				if artist == "" {
					artist = t.fields["Artist"]
				}
				if artist != "" {
					display = title + "  —  " + artist
				}
			}
			searchParts := []string{strings.ToLower(display), strings.ToLower(title)}
			for _, col := range m.layout {
				searchParts = append(searchParts, strings.ToLower(fieldVal(t, col)))
			}
			items = append(items, metaItem{
				display: display,
				key:     t.path,
				sortKey: strings.ToLower(title),
				search:  strings.Join(searchParts, "\x1f"),
			})
		}
		m.allItems = items
		return
	}

	// Intermediate: group matching tracks by current level column
	col := m.layout[m.stage]
	counts := make(map[string]int)
	searchSets := make(map[string]map[string]bool)
	docSets := make(map[string]map[string]bool)
	for _, t := range m.tracks {
		if !m.trackMatchesSelections(t) {
			continue
		}
		for _, val := range m.facetValues(t, col) {
			counts[val]++
			if _, ok := searchSets[val]; !ok {
				seed := map[string]bool{strings.ToLower(val): true}
				for _, sel := range m.selections {
					seed[strings.ToLower(sel)] = true
				}
				searchSets[val] = seed
			}
			if _, ok := docSets[val]; !ok {
				docSets[val] = make(map[string]bool)
			}

			docParts := make([]string, 0, len(m.selections)+1+len(m.layout[m.stage+1:]))
			for _, sel := range m.selections {
				docParts = append(docParts, strings.ToLower(sel))
			}
			docParts = append(docParts, strings.ToLower(val))
			for _, c := range m.layout[m.stage+1:] {
				for _, fv := range m.facetValues(t, c) {
					lfv := strings.ToLower(fv)
					searchSets[val][lfv] = true
					docParts = append(docParts, lfv)
				}
			}
			docSets[val][strings.Join(docParts, "\x1f")] = true
		}
	}

	items := make([]metaItem, 0, len(counts))
	for val, count := range counts {
		display := fmt.Sprintf("%s  (%d)", val, count)
		sk := strings.ToLower(val)
		set := searchSets[val]
		parts := make([]string, 0, len(set)+1)
		parts = append(parts, strings.ToLower(display))
		for s := range set {
			parts = append(parts, s)
		}
		docs := make([]string, 0, len(docSets[val]))
		for d := range docSets[val] {
			docs = append(docs, d)
		}
		items = append(items, metaItem{display: display, key: val, sortKey: sk, search: strings.Join(parts, "\x1f"), docs: docs})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].sortKey < items[j].sortKey
	})
	m.allItems = items
}

// fieldAliases maps user-facing shorthand to one or more CSV column names (OR match).
var fieldAliases = map[string][]string{
	"title":       {"Title"},
	"t":           {"Title"},
	"artist":      {"Album Artist", "Artist"},
	"albumartist": {"Album Artist"},
	"aa":          {"Album Artist"},
	"album":       {"Album"},
	"al":          {"Album"},
	"genre":       {"Genre"},
	"g":           {"Genre"},
	"composer":    {"Composer"},
	"c":           {"Composer"},
	"year":        {"Year"},
	"y":           {"Year"},
	"date":        {"Date Added"},
	"track":       {"Tracknumber"},
	"tn":          {"Tracknumber"},
	"disc":        {"Discnumber"},
}

// parseFilterTokens splits a filter string into (field, value) pairs.
// Unqualified words get field="".
type filterToken struct {
	cols  []string
	value string
	exact bool
}

func parseFilterTokens(q string) []filterToken {
	var tokens []filterToken
	for _, word := range filterWordRE.FindAllString(q, -1) {
		quoted := strings.Contains(word, "\"")
		if i := strings.IndexByte(word, ':'); i > 0 {
			alias := strings.ToLower(word[:i])
			valRaw := word[i+1:]
			if len(valRaw) >= 2 && strings.HasPrefix(valRaw, "\"") && strings.HasSuffix(valRaw, "\"") {
				valRaw = valRaw[1 : len(valRaw)-1]
				quoted = true
			}
			val := strings.ToLower(valRaw)
			if val == "" {
				continue
			}
			if cols, ok := fieldAliases[alias]; ok {
				tokens = append(tokens, filterToken{cols: cols, value: val, exact: quoted})
				continue
			}
		}
		if len(word) >= 2 && strings.HasPrefix(word, "\"") && strings.HasSuffix(word, "\"") {
			word = word[1 : len(word)-1]
			quoted = true
		}
		// No recognised field prefix — plain word matches title/display
		tokens = append(tokens, filterToken{value: strings.ToLower(word), exact: quoted})
	}
	return tokens
}

func (m *metaModel) itemMatchesTokens(item metaItem, tokens []filterToken) bool {
	// Empty/invalid token list should not hide everything.
	if len(tokens) == 0 {
		return true
	}

	itemSearch := item.search
	if itemSearch == "" {
		itemSearch = strings.ToLower(item.display)
	}

	if m.stage == len(m.layout) {
		t, ok := m.tracksByPath[item.key]
		if !ok {
			return false
		}
		for _, tok := range tokens {
			if len(tok.cols) > 0 {
				matched := false
				for _, col := range tok.cols {
					if tokenMatchesText(strings.ToLower(t.fields[col]), tok) {
						matched = true
						break
					}
				}
				if !matched {
					return false
				}
				continue
			}
			if !tokenMatchesText(itemSearch, tok) {
				return false
			}
		}
		return true
	}

	// Non-leaf: match against precomputed searchable branch text.
	plainVals := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if len(tok.cols) > 0 {
			// Field-qualified terms are documented as track-level. Keep a fast,
			// best-effort branch match here.
			if !tokenMatchesText(itemSearch, tok) {
				return false
			}
			continue
		}
		plainVals = append(plainVals, tok.value)
		if !tokenMatchesText(itemSearch, tok) {
			return false
		}
	}

	// For multi-token plain queries, require all tokens to appear in the same
	// descendant path document, avoiding cross-album false positives.
	if len(plainVals) > 1 {
		for _, d := range item.docs {
			ok := true
			for _, tok := range tokens {
				if len(tok.cols) > 0 {
					continue
				}
				if !tokenMatchesText(d, tok) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	return true
}

func (m *metaModel) applyFilter() {
	if m.filter == "" {
		m.items = m.allItems
		m.cursor = 0
		return
	}

	rx, regexMode, regexErr := compileRegexFilter(m.filter)
	if regexMode {
		if regexErr != nil {
			m.items = nil
			m.cursor = 0
			m.status = "Invalid regex: " + regexErr.Error()
			m.isErr = true
			return
		}
		if strings.HasPrefix(m.status, "Invalid regex:") {
			m.status = ""
			m.isErr = false
		}
		var result []metaItem
		for _, item := range m.allItems {
			if m.itemMatchesRegex(item, rx) {
				result = append(result, item)
			}
		}
		m.items = result
		if m.cursor >= len(m.items) {
			m.cursor = 0
		}
		return
	}

	tokens := parseFilterTokens(m.filter)

	var result []metaItem
	for _, item := range m.allItems {
		if m.itemMatchesTokens(item, tokens) {
			result = append(result, item)
		}
	}
	m.items = result
	if m.cursor >= len(m.items) {
		m.cursor = 0
	}
}

func (m *metaModel) restoreCursor() {
	if c, ok := m.stageCursors[m.stage]; ok && c < len(m.items) {
		m.cursor = c
	} else {
		m.cursor = 0
	}
}

func (m *metaModel) drillIn() tea.Cmd {
	if len(m.items) == 0 {
		return nil
	}
	item := m.items[m.cursor]
	m.stageCursors[m.stage] = m.cursor
	parentFilter := m.filter

	if m.stage == len(m.layout) {
		return playCmd(item.key, false)
	}

	m.stageFilters[m.stage] = m.filter // save current filter before descending
	m.selections = append(m.selections, item.key)
	m.stage++
	m.selected = make(map[string]bool) // clear selection when changing stage
	// Prefer the currently active query when drilling in; fall back to this
	// stage's remembered query only when parent has no active filter.
	m.filter = parentFilter
	if m.filter == "" {
		m.filter = m.stageFilters[m.stage] // empty if first visit
	}
	m.filterMode = false
	m.computeAllItems()
	m.applyFilter()
	m.restoreCursor()
	return nil
}

func (m *metaModel) goBack() {
	if m.stage == 0 {
		return
	}
	m.stageFilters[m.stage] = m.filter // save current filter before ascending
	m.stageCursors[m.stage] = m.cursor
	m.stage--
	m.selected = make(map[string]bool) // clear selection when changing stage
	m.selections = m.selections[:m.stage]
	m.filter = m.stageFilters[m.stage] // restore the filter that was active at this stage
	m.filterMode = false
	m.computeAllItems()
	m.applyFilter()
	m.restoreCursor()
}

func (m *metaModel) playCurrent() tea.Cmd {
	if len(m.items) == 0 {
		return nil
	}

	// At leaf with selection: replace queue with selected tracks in list order
	if m.stage == len(m.layout) && len(m.selected) > 0 {
		var paths []string
		for _, item := range m.items {
			if m.selected[item.key] {
				paths = append(paths, item.key)
			}
		}
		snap := paths
		return func() tea.Msg {
			if len(snap) == 0 {
				return statusMsg{text: "No tracks selected", isErr: true}
			}
			mpvLoadFile(pipeName, snap[0], "replace")
			for _, p := range snap[1:] {
				mpvLoadFile(pipeName, p, "append-play")
			}
			return statusMsg{text: fmt.Sprintf("Playing %d selected tracks", len(snap))}
		}
	}
	item := m.items[m.cursor]

	if m.stage == len(m.layout) {
		// Single track at leaf — replace
		return playCmd(item.key, false)
	}

	// Intermediate: collect all matching tracks, replace queue
	targetSels := make([]string, len(m.selections)+1)
	copy(targetSels, m.selections)
	targetSels[len(m.selections)] = item.key
	targetCols := m.layout[:len(targetSels)]

	var paths []string
	for _, t := range m.tracks {
		match := true
		for i, col := range targetCols {
			if !m.trackMatchesFacet(t, col, targetSels[i]) {
				match = false
				break
			}
		}
		if match {
			paths = append(paths, t.path)
		}
	}
	sort.Strings(paths)
	snap := paths
	return func() tea.Msg {
		if len(snap) == 0 {
			return statusMsg{text: "No tracks found", isErr: true}
		}
		mpvLoadFile(pipeName, snap[0], "replace")
		for _, p := range snap[1:] {
			mpvLoadFile(pipeName, p, "append-play")
		}
		return statusMsg{text: fmt.Sprintf("Playing %d tracks", len(snap))}
	}
}

func (m *metaModel) appendCurrent() tea.Cmd {
	if len(m.items) == 0 {
		return nil
	}
	item := m.items[m.cursor]

	if m.stage == len(m.layout) {
		return playCmd(item.key, true)
	}

	// Build the full selection filter: current selections + this item's key
	targetSels := make([]string, len(m.selections)+1)
	copy(targetSels, m.selections)
	targetSels[len(m.selections)] = item.key
	targetCols := m.layout[:len(targetSels)]

	var paths []string
	for _, t := range m.tracks {
		match := true
		for i, col := range targetCols {
			if !m.trackMatchesFacet(t, col, targetSels[i]) {
				match = false
				break
			}
		}
		if match {
			paths = append(paths, t.path)
		}
	}
	sort.Strings(paths)
	snap := paths
	return func() tea.Msg {
		for _, p := range snap {
			mpvLoadFile(pipeName, p, "append-play")
		}
		return statusMsg{text: fmt.Sprintf("Queued %d tracks", len(snap))}
	}
}

func (m *metaModel) switchLayout(delta int) {
	n := len(layouts)
	m.layoutIdx = (m.layoutIdx + n + delta) % n
	m.layout = layouts[m.layoutIdx].Levels
	// Reset to stage 0
	m.stage = 0
	m.selections = nil
	m.stageCursors = make(map[int]int)
	m.stageFilters = make(map[int]string)
	m.selected = make(map[string]bool)
	m.filter = ""
	m.filterMode = false
	m.computeAllItems()
	m.applyFilter()
	m.cursor = 0
	m.status = "Layout: " + layouts[m.layoutIdx].Name
	m.isErr = false
}

func (m *metaModel) breadcrumb() string {
	if !m.loaded {
		if m.loadErr != "" {
			return "[Meta] Error"
		}
		return "[Meta] Loading..."
	}
	parts := []string{"[" + layouts[m.layoutIdx].Name + "]"}
	for i, sel := range m.selections {
		parts = append(parts, m.layout[i]+": "+truncateStr(sel, 30))
	}
	if m.stage < len(m.layout) {
		parts = append(parts, m.layout[m.stage])
	} else {
		parts = append(parts, "Tracks")
	}
	return strings.Join(parts, "  ›  ")
}

func (m metaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		return m, nil

	case metaLoadedMsg:
		if msg.errMsg != "" {
			m.loadErr = msg.errMsg
			m.isErr = true
			m.loaded = true
			return m, nil
		}
		m.tracks = msg.tracks
		m.headers = msg.headers
		m.loaded = true
		// Build path index for fast field lookup in applyFilter
		m.tracksByPath = make(map[string]*track, len(msg.tracks))
		for i := range m.tracks {
			m.tracksByPath[m.tracks[i].path] = &m.tracks[i]
		}

		// Warn about any layout columns missing from the CSV
		colSet := make(map[string]bool, len(msg.headers))
		for _, h := range msg.headers {
			colSet[h] = true
		}
		var missing []string
		for _, col := range m.layout {
			if !colSet[col] {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			m.status = "⚠ columns not in CSV: " + strings.Join(missing, ", ")
			m.isErr = true
		} else {
			m.status = fmt.Sprintf("%d tracks  —  %s", len(m.tracks), layouts[m.layoutIdx].Name)
		}

		m.computeAllItems()
		m.applyFilter()
		return m, nil

	case artMsg:
		if msg.path == m.artPath {
			m.artContent = msg.content
		}
		return m, nil

	case statusMsg:
		if msg.path != "" && msg.path != m.artPath {
			return m, nil
		}
		m.status = msg.text
		m.isErr = msg.isErr
		return m, nil

	case tea.KeyMsg:
		k := msg.String()

		if k == "ctrl+c" {
			return m, tea.Quit
		}

		if m.helpMode {
			switch k {
			case "up", "k", "pgup", "pageup":
				m.helpScroll--
				return m, nil
			case "down", "j", "pgdown", "pagedown":
				m.helpScroll++
				return m, nil
			case "home", "g":
				m.helpScroll = 0
				return m, nil
			case "end", "G":
				m.helpScroll = 1 << 30
				return m, nil
			case "o":
				return m, openConfigCmd(false)
			case "O":
				return m, openConfigCmd(true)
			case "esc", "?", "q", "enter", " ":
				m.helpMode = false
				m.helpScroll = 0
				return m, nil
			}
			return m, nil
		}

		if !m.loaded {
			return m, nil
		}

		if k == "esc" {
			if m.filterMode {
				m.filterMode = false
				m.filter = ""
				m.filterCursor = 0
				m.computeAllItems()
				m.applyFilter()
				m.cursor = 0
			} else if len(m.selected) > 0 {
				m.selected = make(map[string]bool)
				m.status = ""
			}
			return m, nil
		}

		if m.filterMode {
			runes := []rune(m.filter)
			m.filterCursor = clampInt(m.filterCursor, 0, len(runes))
			switch k {
			case "up":
				if m.cursor > 0 {
					m.cursor--
				}
			case "down":
				if m.cursor < len(m.items)-1 {
					m.cursor++
				}
			case "left":
				if m.filterCursor > 0 {
					m.filterCursor--
				}
			case "right":
				if m.filterCursor < len(runes) {
					m.filterCursor++
				}
			case "home":
				m.filterCursor = 0
			case "end":
				m.filterCursor = len(runes)
			case "enter":
				m.filterMode = false
				cmd := m.drillIn()
				return m, cmd
			case " ":
				m.filter = string(runes[:m.filterCursor]) + " " + string(runes[m.filterCursor:])
				m.filterCursor++
				m.applyFilter()
				m.cursor = 0
			case "backspace":
				if m.filterCursor > 0 {
					m.filter = string(runes[:m.filterCursor-1]) + string(runes[m.filterCursor:])
					m.filterCursor--
					m.applyFilter()
					m.cursor = 0
				} else {
					m.filterMode = false
				}
			case "delete":
				if m.filterCursor < len(runes) {
					m.filter = string(runes[:m.filterCursor]) + string(runes[m.filterCursor+1:])
					m.applyFilter()
					m.cursor = 0
				}
			default:
				runes := []rune(k)
				if len(runes) == 1 && runes[0] >= 32 {
					m.filter = string([]rune(m.filter)[:m.filterCursor]) + k + string([]rune(m.filter)[m.filterCursor:])
					m.filterCursor += len(runes)
					m.applyFilter()
					m.cursor = 0
				}
			}
			return m, m.artRefreshCmd()
		}

		// Normal mode
		switch k {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "enter", "right", "l":
			cmd := m.drillIn()
			return m, tea.Batch(cmd, m.artRefreshCmd())
		case "left", "h", "backspace":
			m.goBack()
		case " ":
			if m.stage == len(m.layout) && len(m.items) > 0 {
				key := m.items[m.cursor].key
				if m.selected[key] {
					delete(m.selected, key)
				} else {
					m.selected[key] = true
				}
				if m.cursor < len(m.items)-1 {
					m.cursor++
				}
				if len(m.selected) > 0 {
					m.status = fmt.Sprintf("%d selected  a=queue  r=replace  Esc=clear", len(m.selected))
				} else {
					m.status = ""
				}
			}
		case "ctrl+a":
			if m.stage == len(m.layout) {
				for _, item := range m.items {
					m.selected[item.key] = true
				}
				m.status = fmt.Sprintf("%d selected  a=queue  r=replace  Esc=clear", len(m.selected))
			}
		case "/":
			m.filterMode = true
			m.filterCursor = len([]rune(m.filter))
		case "a":
			cmd := m.appendCurrent()
			return m, cmd
		case "r":
			cmd := m.playCurrent()
			return m, cmd
		case "?":
			m.helpMode = true
			return m, nil
		case "p":
			return m, pauseCmd()
		case "n":
			return m, nextCmd()
		case "N":
			return m, prevCmd()
		case "i":
			artMode = (artMode + 1) % 3
			switch artMode {
			case 0: // artless
				m.artContent = ""
				m.artPath = ""
				return m, openArtViewer("") // kill any mpv viewer
			case 1: // chafa
				m.artPath = ""
				return m, m.artRefreshCmd()
			case 2: // mpv
				m.artContent = ""
				m.artPath = ""
				return m, m.artRefreshCmd()
			}
		case "v":
			return m, openArtViewer(m.artPath)
		case "o":
			return m, openConfigCmd(false)
		case "O":
			return m, openConfigCmd(true)
		case "q":
			return m, tea.Quit
		case "g":
			m.cursor = 0
		case "G":
			if len(m.items) > 0 {
				m.cursor = len(m.items) - 1
			}
		case "L":
			m.switchLayout(+1)
		case "H":
			m.switchLayout(-1)
		case "s":
			m.splitGenres = !m.splitGenres
			m.stage = 0
			m.selections = nil
			m.stageCursors = make(map[int]int)
			m.stageFilters = make(map[int]string)
			m.selected = make(map[string]bool)
			m.filter = ""
			m.filterMode = false
			m.filterCursor = 0
			m.computeAllItems()
			m.applyFilter()
			m.cursor = 0
			if m.splitGenres {
				m.status = "Genre split: on"
			} else {
				m.status = "Genre split: off"
			}
			m.isErr = false
		}
	}

	return m, m.artRefreshCmd()
}

func (m metaModel) View() string {
	var sb strings.Builder

	w := m.width
	if w < 10 {
		w = 80
	}

	if m.helpMode {
		cfgFile := configPath()
		cfgDir := filepath.Dir(cfgFile)
		lines := []string{
			"  Navigation",
			"    ↑↓ / j k       move cursor",
			"    → / l / Enter   drill in / play track",
			"    ← / h / Bksp   go back",
			"    g / G           top / bottom",
			"",
			"  Playback",
			"    Enter           play track / drill in",
			"    r               replace queue with item / selection",
			"    a               append to queue",
			"    p               pause / resume",
			"    n / N           next / previous track",
			"",
			"  Selection  (track level only)",
			"    Space           toggle mark",
			"    ctrl+a          mark all visible",
			"    Esc             clear marks",
			"",
			"  Filter",
			"    /               enter filter mode",
			"    Esc             exit filter  (filter remembered per level)", "    At track level, field:value syntax is supported (multiple = AND):",
			"    Free text matches across active layout columns (all levels):",
			"      Example in Album Artist › Album:  pearl ten",
			"    Quoted terms are exact phrases:  \"dark side\"",
			"    Short numeric terms (e.g. 99) use token boundaries by default",
			"    Regex mode:  re:<pattern>   example: re:\\b99\\b",
			"      artist:   album:   genre:   composer:   year:   date:",
			"      title: t:   albumartist: aa:   track: tn:   disc:",
			"    Example:  artist:bach genre:classical  or  album:london help", "",
			"  Layouts",
			"    L / H           cycle layouts forward / back",
			"    s               toggle split Genre values on ';'",
			"",
			"  Other",
			"    i               toggle album art panel",
			"    o               open config.toml in default editor",
			"    O               open config folder",
			"    config.toml     " + terminalLink(cfgFile, cfgFile),
			"    config folder   " + terminalLink(cfgDir, cfgDir),
			"    Tab             switch to Folder browser",
			"    Esc / ? / q     close help",
			"    q / ctrl+c      quit",
		}
		return renderScrollableHelp("Meta Browser — Key Bindings", lines, w, m.height, m.helpScroll)
	}

	sb.WriteString(stylePath.Render("  "+truncateStr(m.breadcrumb(), w-2)) + "\n")

	// Show chafa art panel only in mode 1, when fully loaded and terminal wide enough.
	showArt := artMode == 1 && m.artContent != "" && w-artW >= artMinListW
	listW := w
	if showArt {
		listW = w - artW
	}
	sb.WriteString(styleDim.Render(strings.Repeat("─", listW)) + "\n")

	if !m.loaded {
		if m.loadErr != "" {
			sb.WriteString(styleErr.Render("  Error: "+m.loadErr) + "\n")
		} else {
			sb.WriteString(styleStatus.Render("  Reading CSV...") + "\n")
		}
		sb.WriteString(styleHelp.Render("  Tab→folders  ctrl+c quit"))
		return sb.String()
	}

	extraLines := 0
	if m.filterMode || m.filter != "" {
		hits := fmt.Sprintf("  %d/%d", len(m.items), len(m.allItems))
		hint := activeFilterHints(m.filter)
		prompt := filterPromptText(m.filter, m.filterCursor, m.filterMode)
		sb.WriteString("  " + styleFilter.Render(prompt) + styleDim.Render(hits+hint) + "\n")
		extraLines = 1
	}

	overhead := 4 + extraLines
	listHeight := m.height - overhead
	if listHeight < 3 {
		listHeight = 3
	}

	start := 0
	if m.cursor >= listHeight {
		start = m.cursor - listHeight + 1
	}
	end := start + listHeight
	if end > len(m.items) {
		end = len(m.items)
	}

	isLeaf := m.stage == len(m.layout)

	for i := start; i < end; i++ {
		item := m.items[i]
		var label string
		if isLeaf {
			if m.selected[item.key] {
				label = "✓ " + item.display
			} else {
				label = "♪ " + item.display
			}
		} else {
			label = "▶ " + item.display
		}

		var line string
		if i == m.cursor {
			padLen := listW - 3 - len([]rune(label))
			if padLen < 0 {
				padLen = 0
			}
			line = "  " + styleSel.Render(" "+label+strings.Repeat(" ", padLen))
		} else if isLeaf && m.selected[item.key] {
			line = "  " + styleMarked.Render(label)
		} else if isLeaf {
			line = "  " + styleFile.Render(label)
		} else {
			line = "  " + styleDir.Render(label)
		}
		sb.WriteString(line + "\n")
	}

	if len(m.items) == 0 {
		if m.filter != "" {
			sb.WriteString(styleStatus.Render("  no matches for \""+m.filter+"\"") + "\n")
		} else {
			sb.WriteString(styleStatus.Render("  (empty)") + "\n")
		}
	}

	for i := end - start; i < listHeight; i++ {
		sb.WriteString("\n")
	}

	status := m.status
	if status == "" {
		pos := ""
		if len(m.items) > 0 {
			pos = fmt.Sprintf("%d/%d", m.cursor+1, len(m.items))
		}
		status = fmt.Sprintf("%d items  %s", len(m.allItems), pos)
	}
	if m.isErr {
		sb.WriteString(styleErr.Render("  "+status) + "\n")
	} else {
		sb.WriteString(styleStatus.Render("  "+status) + "\n")
	}

	artHint := [3]string{"i: art off", "i: chafa\u25b8mpv", "i: mpv\u25b8off"}[artMode]
	var help string
	if m.filterMode {
		help = "  type to filter  ↑↓ navigate  ←→ move cursor  Home/End jump  Enter select  Backspace delete  Del delete  Esc exit filter"
	} else if isLeaf {
		help = "  Enter play  a append  r replace  n/N next/prev  ↑↓/jk  ← back  / filter  L/H layouts  s split-genre  " + artHint + "  v view  Tab→folders  p pause  q quit"
	} else {
		help = "  Enter drill down  a queue all  r replace  n/N next/prev  ↑↓/jk  ← back  / filter  L/H layouts  s split-genre  " + artHint + "  Tab→folders  q quit"
	}
	sb.WriteString(styleHelp.Render(help))

	if showArt {
		return artJoin(sb.String(), m.artContent)
	}
	return sb.String()
}
