package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

type albumRequest struct {
	lineNo int
	raw    string
	artist string
	album  string
}

type albumGroup struct {
	artist     string
	album      string
	artistNorm string
	albumNorm  string
	tracks     []track
}

type matchResult struct {
	req     albumRequest
	group   *albumGroup
	isExact bool
}

type importListOptions struct {
	dryRun        bool
	writePlaylist bool
	writeMisses   bool
}

func resolvedCSVPath(cfg appConfig) string {
	if cfg.Meta.CSVPath != "" {
		return cfg.Meta.CSVPath
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		appdata, _ = os.UserConfigDir()
	}
	return filepath.Join(appdata, "fzmp", "metadata-cache.csv")
}

func normalizeListKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	// Treat '&' and 'and' as equivalent for list-vs-library matching.
	s = strings.ReplaceAll(s, "&", " and ")
	// Fold accents so "deja" can match "déjà".
	s = stripDiacritics(s)
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == '\'' || r == '’' {
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func stripDiacritics(s string) string {
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func parseAlbumListFile(path string) (reqs []albumRequest, invalid []albumRequest, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		artist, album, ok := splitArtistAlbum(line)
		r := albumRequest{lineNo: lineNo, raw: line, artist: artist, album: album}
		if !ok {
			invalid = append(invalid, r)
			continue
		}
		reqs = append(reqs, r)
	}
	if err := s.Err(); err != nil {
		return nil, nil, err
	}
	return reqs, invalid, nil
}

func splitArtistAlbum(line string) (artist, album string, ok bool) {
	delims := []string{" - ", " – ", " — ", " | "}
	for _, d := range delims {
		if strings.Contains(line, d) {
			parts := strings.SplitN(line, d, 2)
			if len(parts) == 2 {
				artist = strings.TrimSpace(parts[0])
				album = strings.TrimSpace(parts[1])
				if artist != "" && album != "" {
					return artist, album, true
				}
			}
		}
	}
	return "", "", false
}

func buildAlbumGroups(tracks []track) []albumGroup {
	groups := make(map[string]*albumGroup)
	for _, t := range tracks {
		artist := strings.TrimSpace(t.fields["Album Artist"])
		if artist == "" {
			artist = strings.TrimSpace(t.fields["Artist"])
		}
		album := strings.TrimSpace(t.fields["Album"])
		if artist == "" || album == "" {
			continue
		}
		artistNorm := normalizeListKey(artist)
		albumNorm := normalizeListKey(album)
		if artistNorm == "" || albumNorm == "" {
			continue
		}
		key := artistNorm + "|" + albumNorm
		g := groups[key]
		if g == nil {
			g = &albumGroup{artist: artist, album: album, artistNorm: artistNorm, albumNorm: albumNorm}
			groups[key] = g
		}
		g.tracks = append(g.tracks, t)
	}

	out := make([]albumGroup, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.tracks, func(i, j int) bool {
			di, dj := trackNum(g.tracks[i], "Discnumber"), trackNum(g.tracks[j], "Discnumber")
			if di != dj {
				return di < dj
			}
			ti, tj := trackNum(g.tracks[i], "Tracknumber"), trackNum(g.tracks[j], "Tracknumber")
			if ti != tj {
				return ti < tj
			}
			return strings.ToLower(g.tracks[i].fields["Title"]) < strings.ToLower(g.tracks[j].fields["Title"])
		})
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		ai := strings.ToLower(out[i].artist + "|" + out[i].album)
		aj := strings.ToLower(out[j].artist + "|" + out[j].album)
		return ai < aj
	})
	return out
}

func buildExactAlbumGroupIndex(groups []albumGroup) map[string]*albumGroup {
	idx := make(map[string]*albumGroup, len(groups))
	for i := range groups {
		key := groups[i].artistNorm + "|" + groups[i].albumNorm
		idx[key] = &groups[i]
	}
	return idx
}

func scorePart(needle, hay string) int {
	if needle == "" || hay == "" {
		return 0
	}
	if needle == hay {
		return 400
	}
	if strings.HasPrefix(hay, needle) || strings.HasPrefix(needle, hay) {
		return 260
	}
	if strings.Contains(hay, needle) || strings.Contains(needle, hay) {
		return 140
	}
	return 0
}

func hasTokenOverlap(needle, hay string) bool {
	ns := strings.Fields(needle)
	if len(ns) == 0 {
		return false
	}
	hs := make(map[string]bool)
	for _, w := range strings.Fields(hay) {
		if len(w) >= 2 {
			hs[w] = true
		}
	}
	for _, w := range ns {
		if len(w) < 2 {
			continue
		}
		if hs[w] {
			return true
		}
	}
	return false
}

func findBestGroup(req albumRequest, groups []albumGroup, exactIdx map[string]*albumGroup) (*albumGroup, bool) {
	ra := normalizeListKey(req.artist)
	rl := normalizeListKey(req.album)
	if ra == "" || rl == "" {
		return nil, false
	}

	if g, ok := exactIdx[ra+"|"+rl]; ok {
		return g, true
	}

	bestIdx := -1
	bestScore := -1
	bestPenalty := 1 << 30
	for i := range groups {
		ga := groups[i].artistNorm
		gl := groups[i].albumNorm
		sa := scorePart(ra, ga)
		sl := scorePart(rl, gl)
		if sa == 0 || sl == 0 {
			continue
		}
		if !hasTokenOverlap(ra, ga) || !hasTokenOverlap(rl, gl) {
			continue
		}
		if sa < 200 && sl < 200 {
			continue
		}
		score := sa + sl
		penalty := absInt(len(ga)-len(ra)) + absInt(len(gl)-len(rl))
		if score > bestScore || (score == bestScore && penalty < bestPenalty) {
			bestIdx = i
			bestScore = score
			bestPenalty = penalty
		}
	}
	if bestIdx < 0 {
		return nil, false
	}
	return &groups[bestIdx], false
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func importOutputDir(cfg appConfig, csvPath string) string {
	if cfg.MusicRoot != "" {
		if info, err := os.Stat(cfg.MusicRoot); err == nil && info.IsDir() {
			return cfg.MusicRoot
		}
	}
	if csvPath != "" {
		d := filepath.Dir(csvPath)
		if d != "" {
			return d
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func writePlaylistFile(path string, queuePaths []string) error {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, p := range queuePaths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func writeMissesFile(path string, results []matchResult, invalid []albumRequest) error {
	var b strings.Builder
	for _, mr := range results {
		if mr.group != nil {
			continue
		}
		b.WriteString(fmt.Sprintf("MISS line %d: %s\n", mr.req.lineNo, mr.req.raw))
	}
	for _, bad := range invalid {
		b.WriteString(fmt.Sprintf("SKIP line %d: %s\n", bad.lineNo, bad.raw))
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func runImportListMode(listPath string, cfg appConfig, opts importListOptions) error {
	csvPath := resolvedCSVPath(cfg)
	tracks, _, err := loadCSV(csvPath)
	if err != nil {
		return fmt.Errorf("load csv %q: %w", csvPath, err)
	}

	reqs, invalid, err := parseAlbumListFile(listPath)
	if err != nil {
		return fmt.Errorf("read list %q: %w", listPath, err)
	}
	if len(reqs) == 0 && len(invalid) == 0 {
		fmt.Println("No usable lines found in list file.")
		return nil
	}

	groups := buildAlbumGroups(tracks)
	if len(groups) == 0 {
		return fmt.Errorf("no albums found in CSV metadata")
	}
	exactIdx := buildExactAlbumGroupIndex(groups)

	results := make([]matchResult, 0, len(reqs))
	for _, r := range reqs {
		g, exact := findBestGroup(r, groups, exactIdx)
		results = append(results, matchResult{req: r, group: g, isExact: exact})
	}

	var queuePaths []string
	matched := 0
	for _, mr := range results {
		if mr.group == nil {
			continue
		}
		matched++
		for _, t := range mr.group.tracks {
			queuePaths = append(queuePaths, t.path)
		}
	}

	if !opts.dryRun && len(queuePaths) > 0 {
		if err := mpvLoadFile(pipeName, queuePaths[0], "replace"); err != nil {
			return err
		}
		for _, p := range queuePaths[1:] {
			if err := mpvLoadFile(pipeName, p, "append-play"); err != nil {
				return err
			}
		}
	}

	var playlistPath string
	var missesPath string
	if opts.writePlaylist || opts.writeMisses {
		outDir := importOutputDir(cfg, csvPath)
		stem := strings.TrimSuffix(filepath.Base(listPath), filepath.Ext(listPath))
		if stem == "" {
			stem = "import"
		}
		if opts.writePlaylist {
			playlistPath = filepath.Join(outDir, stem+".m3u8")
			if err := writePlaylistFile(playlistPath, queuePaths); err != nil {
				return fmt.Errorf("write playlist %q: %w", playlistPath, err)
			}
		}
		if opts.writeMisses {
			missesPath = filepath.Join(outDir, stem+".misses.txt")
			if err := writeMissesFile(missesPath, results, invalid); err != nil {
				return fmt.Errorf("write misses %q: %w", missesPath, err)
			}
		}
	}

	fmt.Printf("List import: %s\n", listPath)
	fmt.Printf("CSV source:  %s\n", csvPath)
	fmt.Printf("Matched: %d/%d lines\n", matched, len(reqs))
	fmt.Printf("Queued tracks: %d\n", len(queuePaths))
	if playlistPath != "" {
		fmt.Printf("Playlist file: %s\n", playlistPath)
	}
	if missesPath != "" {
		fmt.Printf("Misses file:   %s\n", missesPath)
	}
	if opts.dryRun {
		fmt.Println("Dry run mode: nothing sent to mpv.")
	}

	for _, mr := range results {
		if mr.group != nil {
			kind := "fuzzy"
			if mr.isExact {
				kind = "exact"
			}
			fmt.Printf("OK   line %d: %s -> %s - %s (%d tracks, %s)\n", mr.req.lineNo, mr.req.raw, mr.group.artist, mr.group.album, len(mr.group.tracks), kind)
		} else {
			fmt.Printf("MISS line %d: %s\n", mr.req.lineNo, mr.req.raw)
		}
	}
	for _, bad := range invalid {
		fmt.Printf("SKIP line %d: %s\n", bad.lineNo, bad.raw)
	}

	return nil
}
