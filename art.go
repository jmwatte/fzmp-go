package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"fzmp-go/mpv"

	tea "github.com/charmbracelet/bubbletea"
)

// artW is the width of the art panel in terminal columns.
// artH is the height — roughly artW/2 because terminal chars are ~2:1 (pixel height:width).
const artW = 38
const artH = 19

// artMinListW is the minimum list-panel width before art is suppressed.
const artMinListW = 30

// truncateStr truncates s to at most max runes, appending … if truncated.
func truncateStr(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

var artCandidates = []string{
	"folder.jpg", "folder.jpeg", "folder.png",
	"cover.jpg", "cover.jpeg", "cover.png",
	"front.jpg", "front.jpeg", "front.png",
	"album.jpg", "album.jpeg", "album.png",
}

// artMsg carries a rendered chafa image string back to the model.
type artMsg struct {
	content string
	path    string // path that was rendered (used to discard stale results)
}

// noArtPath is the placeholder image shown when no album art exists in a
// directory.  Set once at startup from config.  Empty = no placeholder.
var noArtPath string

// mpvPath, chafaPath, ffmpegPath are executable paths; default to PATH resolution.
var (
	mpvPath             = "mpv"
	chafaPath           = "chafa"
	ffmpegPath          = "ffmpeg"
	artViewerAutoResize = true
	artViewerSize       = "800x800"
)

// artMode is the shared art display mode across all views.
// 0 = none, 1 = chafa (terminal column), 2 = mpv (external window).
var artMode int

// findArtInDir looks for a cover image directly in dir (no noArtPath fallback).
func findArtInDir(dir string) string {
	for _, name := range artCandidates {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// findArtPath returns the first recognised art file in dir, falling back to
// noArtPath when nothing is found.
func findArtPath(dir string) string {
	if dir != "" {
		if p := findArtInDir(dir); p != "" {
			return p
		}
	}
	return noArtPath
}

// findSubdirArtPaths collects cover images from immediate subdirectories of dir.
// Used to build a mosaic when the directory itself has no direct cover art.
func findSubdirArtPaths(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if p := findArtInDir(filepath.Join(dir, e.Name())); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// artChafaForDir returns (key, cmd) for chafa rendering of dir.
// Tries a direct cover first, then a subdirectory mosaic, then noArtPath.
func artChafaForDir(dir string) (key string, cmd tea.Cmd) {
	if dir != "" {
		if p := findArtInDir(dir); p != "" {
			return p, renderArtCmd(p)
		}
		paths := findSubdirArtPaths(dir)
		if len(paths) == 1 {
			return paths[0], renderArtCmd(paths[0])
		}
		if len(paths) > 1 {
			key = strings.Join(paths, "|")
			return key, renderArtGridCmd(paths)
		}
	}
	if noArtPath != "" {
		return noArtPath, renderArtCmd(noArtPath)
	}
	return "", nil
}

// mosaicCachePath returns the path where the composite mosaic image for the
// given set of cover paths should be cached.  Uses a hash of the sorted paths
// so the same artist always gets the same cache file.
func mosaicCachePath(paths []string) string {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)
	sum := md5.Sum([]byte(strings.Join(sorted, "\n")))
	name := hex.EncodeToString(sum[:]) + ".jpg"
	return filepath.Join(os.TempDir(), "fzmp-art-cache", name)
}

const (
	mosaicCellSize       = 256
	mosaicChunkSize      = 16
	mosaicCmdMaxEstimate = 30000
)

func mosaicGrid(n int) (cols, rows, total int) {
	if n < 1 {
		return 1, 1, 1
	}
	cols = 1
	for cols*cols < n {
		cols++
	}
	rows = (n + cols - 1) / cols
	total = cols * rows
	return cols, rows, total
}

func mosaicFilter(n, cols, cellSize int) string {
	scaleFilter := fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
		cellSize, cellSize, cellSize, cellSize,
	)

	var filter strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&filter, "[%d:v]%s[v%d];", i, scaleFilter, i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&filter, "[v%d]", i)
	}
	fmt.Fprintf(&filter, "xstack=inputs=%d:layout=", n)
	for i := 0; i < n; i++ {
		if i > 0 {
			filter.WriteString("|")
		}
		fmt.Fprintf(&filter, "%d_%d", (i%cols)*cellSize, (i/cols)*cellSize)
	}
	filter.WriteString(":shortest=1[out]")
	return filter.String()
}

func ensureBlackCell(outPath string, cellSize int) (string, error) {
	blackCell := filepath.Join(filepath.Dir(outPath), "black-cell.png")
	if _, err := os.Stat(blackCell); err == nil {
		return blackCell, nil
	}

	cmd := exec.Command(ffmpegPath, "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("color=black:size=%dx%d:rate=1", cellSize, cellSize),
		"-frames:v", "1", blackCell)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("ffmpeg black-cell failed: %w", err)
		}
		return "", fmt.Errorf("ffmpeg black-cell failed: %w: %s", err, msg)
	}

	return blackCell, nil
}

func estimateMosaicCmdLen(paths []string, outPath string) int {
	if len(paths) == 0 {
		return 0
	}
	cols, _, total := mosaicGrid(len(paths))
	blackCell := filepath.Join(filepath.Dir(outPath), "black-cell.png")

	args := []string{"-y"}
	for _, p := range paths {
		args = append(args, "-i", p)
	}
	for i := len(paths); i < total; i++ {
		args = append(args, "-i", blackCell)
	}

	args = append(args, "-filter_complex", mosaicFilter(total, cols, mosaicCellSize),
		"-map", "[out]", "-frames:v", "1", outPath)

	n := len(ffmpegPath)
	for _, a := range args {
		n += 1 + len(a)
	}
	return n
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func buildMosaicImageDirect(paths []string, outPath string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no images to mosaic")
	}
	if len(paths) == 1 {
		if filepath.Clean(paths[0]) == filepath.Clean(outPath) {
			return nil
		}
		return copyFile(paths[0], outPath)
	}

	cols, _, total := mosaicGrid(len(paths))
	padded := make([]string, 0, total)
	padded = append(padded, paths...)

	if total > len(paths) {
		blackCell, err := ensureBlackCell(outPath, mosaicCellSize)
		if err != nil {
			return err
		}
		for i := len(paths); i < total; i++ {
			padded = append(padded, blackCell)
		}
	}

	args := []string{"-y"}
	for _, p := range padded {
		args = append(args, "-i", p)
	}

	args = append(args, "-filter_complex", mosaicFilter(len(padded), cols, mosaicCellSize),
		"-map", "[out]", "-frames:v", "1", outPath)

	cmd := exec.Command(ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("ffmpeg mosaic failed: %w", err)
		}
		return fmt.Errorf("ffmpeg mosaic failed: %w: %s", err, msg)
	}
	return nil
}

func buildMosaicImageChunked(paths []string, outPath string) error {
	stageDir, err := os.MkdirTemp(filepath.Dir(outPath), "mosaic-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)

	current := append([]string(nil), paths...)
	for level := 0; ; level++ {
		if len(current) == 0 {
			return fmt.Errorf("no images to mosaic")
		}
		if estimateMosaicCmdLen(current, outPath) <= mosaicCmdMaxEstimate {
			return buildMosaicImageDirect(current, outPath)
		}

		next := make([]string, 0, (len(current)+mosaicChunkSize-1)/mosaicChunkSize)
		for i, chunkIdx := 0, 0; i < len(current); chunkIdx++ {
			chunkOut := filepath.Join(stageDir, fmt.Sprintf("lvl%02d_%04d.jpg", level, chunkIdx))
			end := i + mosaicChunkSize
			if end > len(current) {
				end = len(current)
			}
			for end > i+1 && estimateMosaicCmdLen(current[i:end], chunkOut) > mosaicCmdMaxEstimate {
				end--
			}
			if err := buildMosaicImageDirect(current[i:end], chunkOut); err != nil {
				return err
			}
			next = append(next, chunkOut)
			i = end
		}
		current = next
	}
}

// buildMosaicImage tiles paths into a single composite JPEG at outPath using
// ffmpeg xstack.  Each cell is scaled to cellSize×cellSize (letterboxed).
// Empty grid cells are filled with a cached black PNG so they aren't green.
func buildMosaicImage(paths []string, outPath string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no images to mosaic")
	}

	if estimateMosaicCmdLen(paths, outPath) <= mosaicCmdMaxEstimate {
		return buildMosaicImageDirect(paths, outPath)
	}

	return buildMosaicImageChunked(paths, outPath)
}

// renderArtGridCmd builds a mosaic from paths (via ffmpeg, disk-cached) then
// renders it with chafa at the normal art panel size.
func renderArtGridCmd(paths []string) tea.Cmd {
	return func() tea.Msg {
		key := strings.Join(paths, "|")

		cachePath := mosaicCachePath(paths)
		if err := ensureMosaicCached(paths, cachePath); err != nil {
			return statusMsg{text: "mosaic build failed: " + err.Error(), isErr: true, path: key}
		}

		size := fmt.Sprintf("%dx%d", artW, artH)
		out, err := exec.Command(chafaPath, "--format", "symbols", "--size", size, cachePath).Output()
		if err != nil {
			return statusMsg{text: "mosaic render failed: " + err.Error(), isErr: true, path: key}
		}
		content := strings.ReplaceAll(string(out), "\r", "")
		content = ansiEraseRe.ReplaceAllString(content, "")
		return artMsg{content: content, path: key}
	}
}

// ansiEraseRe matches ANSI sequences that erase screen content or control the
// cursor — these must be stripped from chafa output before embedding art lines
// alongside list text, otherwise they erase the list content to the right.
var ansiEraseRe = regexp.MustCompile(`\x1b\[(?:[0-9;]*[JK]|\?25[lh])`)

func isTransientMosaicStatus(text string, isErr bool) bool {
	if !isErr {
		return false
	}
	return strings.HasPrefix(text, "mosaic build failed:") || strings.HasPrefix(text, "mosaic render failed:")
}

// renderArtCmd runs chafa in a goroutine and returns an artMsg.
func renderArtCmd(path string) tea.Cmd {
	return func() tea.Msg {
		if path == "" {
			return artMsg{}
		}
		size := fmt.Sprintf("%dx%d", artW, artH)
		out, err := exec.Command(chafaPath,
			"--format", "symbols",
			"--size", size,
			path,
		).Output()
		if err != nil {
			return artMsg{path: path} // path set, content empty = error/not-renderable
		}
		// Strip \r (Windows \r\n line endings from chafa) and ANSI erase/cursor-
		// control sequences. The \r would reset the cursor to col 0, causing list
		// text (written after the art) to overwrite the art instead of following it.
		content := strings.ReplaceAll(string(out), "\r", "")
		content = ansiEraseRe.ReplaceAllString(content, "")
		return artMsg{content: content, path: path}
	}
}

// artJoin places artContent on the left and listStr on the right, line by line.
// Each art line is concatenated directly with the corresponding list line, avoiding
// lipgloss.JoinHorizontal which mis-measures blank-space lines as zero-width and
// causes rows beyond the art height to collapse back to column 0.
func artJoin(listStr, artContent string) string {
	listLines := strings.Split(listStr, "\n")

	var artLines []string
	if artContent != "" {
		artLines = strings.Split(artContent, "\n")
		// chafa appends a trailing \n → drop the resulting trailing empty element.
		for len(artLines) > 0 && artLines[len(artLines)-1] == "" {
			artLines = artLines[:len(artLines)-1]
		}
	}

	blank := strings.Repeat(" ", artW)
	result := make([]string, len(listLines))
	for i, ll := range listLines {
		if i < len(artLines) {
			// \033[0m resets art colours so they don't bleed into the list line.
			result[i] = artLines[i] + "\033[0m" + ll
		} else {
			result[i] = blank + ll
		}
	}
	return strings.Join(result, "\n")
}

// artPipeName is the named pipe used by the persistent mpv art viewer.
const artPipeName = "fzmp-art"

// artViewerProc is the currently running mpv art viewer instance (if any).
var (
	artViewerProc *os.Process
	artViewerMu   sync.Mutex
	mosaicBuildMu sync.Mutex
	mosaicBuilds  = make(map[string]chan struct{})
)

// ensureMosaicCached guarantees only one build per cachePath runs at once.
// Concurrent callers wait for the active build and then reuse its result.
func ensureMosaicCached(paths []string, cachePath string) error {
	if _, err := os.Stat(cachePath); err == nil {
		return nil
	}

	for {
		mosaicBuildMu.Lock()
		waitCh, inFlight := mosaicBuilds[cachePath]
		if !inFlight {
			waitCh = make(chan struct{})
			mosaicBuilds[cachePath] = waitCh
			mosaicBuildMu.Unlock()

			var buildErr error
			if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
				buildErr = err
			} else {
				buildErr = buildMosaicImage(paths, cachePath)
			}

			mosaicBuildMu.Lock()
			delete(mosaicBuilds, cachePath)
			close(waitCh)
			mosaicBuildMu.Unlock()

			return buildErr
		}
		mosaicBuildMu.Unlock()

		// Another goroutine is already building this exact mosaic. Wait for it,
		// then use the resulting cache file if available.
		<-waitCh
		if _, err := os.Stat(cachePath); err == nil {
			return nil
		}
		// Previous build failed; retry as the new builder.
	}
}

// openArtViewer sends a loadfile command to an already-running mpv art viewer
// via IPC so the window keeps focus.  If mpv isn't running yet it starts it
// with the image as the initial file.  Passing artPath="" kills the viewer.
func openArtViewer(artPath string) tea.Cmd {
	return func() tea.Msg {
		if artPath == "" {
			artViewerMu.Lock()
			if artViewerProc != nil {
				artViewerProc.Kill()
				artViewerProc = nil
			}
			artViewerMu.Unlock()
			return nil
		}

		artViewerMu.Lock()
		proc := artViewerProc
		artViewerMu.Unlock()

		if proc != nil {
			// Viewer already open — send loadfile via IPC (no new window, no focus steal).
			if err := mpv.LoadFile(artPipeName, artPath, "replace"); err == nil {
				return nil
			}
			// Process died without us knowing; fall through to restart.
			artViewerMu.Lock()
			artViewerProc = nil
			artViewerMu.Unlock()
		}

		// Start a fresh mpv art viewer with the IPC pipe so subsequent images
		// can be sent without reopening the window.
		args := []string{
			"--no-audio",
			"--image-display-duration=inf",
			"--title=fzmp art viewer",
			"--no-osc",
			"--no-terminal",
			"--input-ipc-server=" + `\\.\pipe\` + artPipeName,
		}
		if artViewerAutoResize {
			args = append(args, "--autofit="+artViewerSize)
		} else {
			args = append(args, "--auto-window-resize=no", "--geometry="+artViewerSize)
		}
		cmd := exec.Command(mpvPath, append(args, artPath)...)
		if err := cmd.Start(); err != nil {
			return statusMsg{text: "art viewer: " + err.Error(), isErr: true}
		}
		artViewerMu.Lock()
		artViewerProc = cmd.Process
		artViewerMu.Unlock()

		// Give mpv a moment to create the pipe before any immediately following
		// loadfile calls (e.g. rapid cursor movement during startup).
		time.Sleep(300 * time.Millisecond)
		return nil
	}
}

// openArtViewerForDir returns (key, cmd) for mpv art viewer mode, mirroring
// artChafaForDir: single cover is opened directly; multiple subdir covers are
// tiled into a cached ffmpeg mosaic first, then opened.
func openArtViewerForDir(dir string) (key string, cmd tea.Cmd) {
	if dir != "" {
		if p := findArtInDir(dir); p != "" {
			return p, openArtViewer(p)
		}
		paths := findSubdirArtPaths(dir)
		if len(paths) == 1 {
			return paths[0], openArtViewer(paths[0])
		}
		if len(paths) > 1 {
			key = strings.Join(paths, "|")
			cachePath := mosaicCachePath(paths)
			return key, openArtViewerMosaic(paths, cachePath)
		}
	}
	if noArtPath != "" {
		return noArtPath, openArtViewer(noArtPath)
	}
	return "", nil
}

// openArtViewerMosaic builds the mosaic (if not cached) then opens it in the
// persistent mpv art viewer.
func openArtViewerMosaic(paths []string, cachePath string) tea.Cmd {
	return func() tea.Msg {
		key := strings.Join(paths, "|")
		if err := ensureMosaicCached(paths, cachePath); err != nil {
			return statusMsg{text: "mosaic build failed: " + err.Error(), isErr: true, path: key}
		}
		return openArtViewer(cachePath)()
	}
}
