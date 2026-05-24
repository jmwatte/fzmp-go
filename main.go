package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"fzmp-go/mpv"
)

// pipeName and csvPath are set at startup from config.
var pipeName string

var audioExts = map[string]bool{
	".mp3": true, ".flac": true, ".ogg": true, ".wav": true,
	".m4a": true, ".opus": true, ".wma": true, ".aac": true,
	".ape": true, ".wv": true, ".mka": true,
}

var (
	styleDir    = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleFile   = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	styleSel    = lipgloss.NewStyle().Background(lipgloss.Color("236")).Bold(true)
	styleMarked = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleBadge  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	styleFilter = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleStatus = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	styleHelp   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stylePath   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// statusMsg is shared between folder and meta models.
type statusMsg struct {
	text  string
	isErr bool
}

// --- shared mpv commands ---

func mpvLoadFile(pipe, path, mode string) error {
	return mpv.LoadFile(pipe, path, mode)
}

func playCmd(path string, appendMode bool) tea.Cmd {
	return func() tea.Msg {
		mode := "replace"
		if appendMode {
			mode = "append-play"
		}
		if err := mpv.LoadFile(pipeName, path, mode); err != nil {
			return statusMsg{text: "mpv: " + err.Error(), isErr: true}
		}
		verb := "Playing"
		if appendMode {
			verb = "Queued"
		}
		return statusMsg{text: verb + ": " + filepath.Base(path)}
	}
}

func pauseCmd() tea.Cmd {
	return func() tea.Msg {
		if err := mpv.Pause(pipeName); err != nil {
			return statusMsg{text: "mpv: " + err.Error(), isErr: true}
		}
		return statusMsg{text: "Toggled pause"}
	}
}

func nextCmd() tea.Cmd {
	return func() tea.Msg {
		if err := mpv.Next(pipeName); err != nil {
			return statusMsg{text: "mpv: " + err.Error(), isErr: true}
		}
		return statusMsg{text: "Next track"}
	}
}

func prevCmd() tea.Cmd {
	return func() tea.Msg {
		if err := mpv.Prev(pipeName); err != nil {
			return statusMsg{text: "mpv: " + err.Error(), isErr: true}
		}
		return statusMsg{text: "Previous track"}
	}
}

// --- app model ---

type appModel struct {
	mode   int // 0 = folder, 1 = meta
	folder folderModel
	meta   metaModel
}

func (a appModel) Init() tea.Cmd { return nil }

func (a appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// WindowSizeMsg goes to both sub-models
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		newF, _ := a.folder.Update(ws)
		a.folder = newF.(folderModel)
		newM, _ := a.meta.Update(ws)
		a.meta = newM.(metaModel)
		return a, nil
	}

	// Intercept Tab to switch views
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "tab" {
		a.mode = 1 - a.mode
		if a.mode == 1 && !a.meta.loaded && a.meta.loadErr == "" {
			return a, loadMetaCmd(a.meta.csvPath)
		}
		return a, nil
	}

	// Forward to active sub-model
	if a.mode == 0 {
		newF, cmd := a.folder.Update(msg)
		a.folder = newF.(folderModel)
		return a, cmd
	}
	newM, cmd := a.meta.Update(msg)
	a.meta = newM.(metaModel)
	return a, cmd
}

func (a appModel) View() string {
	if a.mode == 0 {
		return a.folder.View()
	}
	return a.meta.View()
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Config error: %v\n", err)
		// non-fatal: continue with defaults
	}

	var importListPath string
	var dryRunImport bool
	var writePlaylist bool
	var writeMisses bool
	flag.StringVar(&importListPath, "import-list", "", "Path to text file with lines like 'Artist - Album'")
	flag.BoolVar(&dryRunImport, "dry-run", false, "When used with --import-list, print matches without queueing in mpv")
	flag.BoolVar(&writePlaylist, "write-playlist", false, "When used with --import-list, write an .m3u8 file")
	flag.BoolVar(&writeMisses, "write-misses", false, "When used with --import-list, write a misses report file")
	flag.Parse()

	// Apply config globals
	pipeName = cfg.PipeName
	noArtPath = cfg.NoArtImage
	mpvPath = cfg.MpvPath
	chafaPath = cfg.ChafaPath
	ffmpegPath = cfg.FfmpegPath

	if importListPath != "" {
		opts := importListOptions{
			dryRun:        dryRunImport,
			writePlaylist: writePlaylist,
			writeMisses:   writeMisses,
		}
		if err := runImportListMode(importListPath, cfg, opts); err != nil {
			fmt.Fprintf(os.Stderr, "Import error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// CLI arg overrides music_root
	root := cfg.MusicRoot
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Not a directory: %s\n", root)
		os.Exit(1)
	}

	// Write default config if none exists
	cp := configPath()
	if _, err := os.Stat(cp); os.IsNotExist(err) {
		_ = writeDefaultConfig(cp)
	}

	app := appModel{
		folder: initialFolderModel(root),
		meta:   initialMetaModel(cfg),
	}

	p := tea.NewProgram(app, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
