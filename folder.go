package main

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type entry struct {
	name  string
	isDir bool
	path  string
}

type folderModel struct {
	dir           string
	allEntries    []entry
	entries       []entry
	cursor        int
	filter        string
	filterMode    bool
	helpMode      bool
	selected      map[string]bool
	history       map[string]int
	filterHistory map[string]string
	status        string
	isErr         bool
	height        int
	width         int

	artContent string
	artPath    string // path last submitted for rendering (stale-result guard)
}

type loadedMsg struct {
	dir     string
	entries []entry
}

// artRefreshCmd computes the art path for the current cursor position and
// dispatches the appropriate command for the current art mode.
// Returns nil when art is off or the path hasn't changed.
func (m *folderModel) artRefreshCmd() tea.Cmd {
	if artMode == 0 {
		return nil
	}
	var dir string
	if len(m.entries) > 0 && m.cursor < len(m.entries) {
		e := m.entries[m.cursor]
		if e.isDir {
			dir = e.path
		} else {
			dir = m.dir
		}
	} else {
		dir = m.dir
	}
	// Do NOT clear artContent here — keep displaying the previous art while the
	// new one loads, eliminating the layout-jump caused by briefly hiding the art
	// column.  artContent will be replaced when the artMsg arrives.
	switch artMode {
	case 1:
		key, cmd := artChafaForDir(dir)
		if key == m.artPath || cmd == nil {
			return nil
		}
		m.artPath = key
		return cmd
	case 2:
		key, cmd := openArtViewerForDir(dir)
		if key == m.artPath || cmd == nil {
			return nil
		}
		m.artPath = key
		return cmd
	}
	return nil
}

func (m *folderModel) applyFilter() {
	if m.filter == "" {
		m.entries = m.allEntries
		return
	}
	q := strings.ToLower(m.filter)
	var result []entry
	for _, e := range m.allEntries {
		if strings.Contains(strings.ToLower(e.name), q) {
			result = append(result, e)
		}
	}
	m.entries = result
	if m.cursor >= len(m.entries) {
		m.cursor = 0
	}
}

func (m *folderModel) navigate(dest string) tea.Cmd {
	m.history[m.dir] = m.cursor
	m.filterHistory[m.dir] = m.filter
	return navigateCmd(dest)
}

func initialFolderModel(root string) folderModel {
	entries, err := readDir(root)
	m := folderModel{
		dir:           root,
		allEntries:    entries,
		entries:       entries,
		selected:      make(map[string]bool),
		history:       make(map[string]int),
		filterHistory: make(map[string]string),
		height:        24,
	}
	if err != nil {
		m.status = "Error: " + err.Error()
		m.isErr = true
	}
	return m
}

func (m folderModel) Init() tea.Cmd { return nil }

func (m folderModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		return m, nil

	case artMsg:
		if msg.path == m.artPath {
			m.artContent = msg.content
		}
		return m, nil

	case loadedMsg:
		m.dir = msg.dir
		m.allEntries = msg.entries
		m.filter = m.filterHistory[msg.dir]
		m.filterMode = false
		m.applyFilter()
		if pos, ok := m.history[msg.dir]; ok && pos < len(m.entries) {
			m.cursor = pos
		} else {
			m.cursor = 0
		}
		m.status = ""
		m.isErr = false
		return m, m.artRefreshCmd()

	case statusMsg:
		m.status = msg.text
		m.isErr = msg.isErr
		return m, nil

	case tea.KeyMsg:
		k := msg.String()

		if k == "ctrl+c" {
			return m, tea.Quit
		}

		// Any key closes help overlay
		if m.helpMode {
			switch k {
			case "o":
				return m, openConfigCmd(false)
			case "O":
				return m, openConfigCmd(true)
			case "esc", "?", "q", "enter", " ":
				m.helpMode = false
				return m, nil
			}
			return m, nil
		}

		if k == "esc" {
			if m.filterMode {
				m.filterMode = false
				m.filter = ""
				m.applyFilter()
				m.cursor = 0
			} else if len(m.selected) > 0 {
				m.selected = make(map[string]bool)
				m.status = ""
			}
			return m, nil
		}

		if m.filterMode {
			switch k {
			case "up":
				if m.cursor > 0 {
					m.cursor--
				}
				return m, m.artRefreshCmd()
			case "down":
				if m.cursor < len(m.entries)-1 {
					m.cursor++
				}
				return m, m.artRefreshCmd()
			case "enter", "right":
				if len(m.entries) == 0 {
					return m, nil
				}
				if len(m.selected) > 0 {
					cmd := enqueueSelectedCmd(m.selected, m.allEntries)
					m.selected = make(map[string]bool)
					m.status = "Queuing..."
					m.filterMode = false
					return m, cmd
				}
				e := m.entries[m.cursor]
				m.filterMode = false
				if e.isDir {
					return m, m.navigate(e.path)
				}
				return m, playCmd(e.path, false)
			case " ":
				m.filter += " "
				m.applyFilter()
				m.cursor = 0
				return m, nil
			case "backspace":
				if m.filter != "" {
					runes := []rune(m.filter)
					m.filter = string(runes[:len(runes)-1])
					m.applyFilter()
				} else {
					m.filterMode = false
				}
				return m, nil
			default:
				runes := []rune(k)
				if len(runes) == 1 && runes[0] >= 32 {
					m.filter += k
					m.applyFilter()
					m.cursor = 0
				}
				return m, nil
			}
		}

		// Normal mode
		switch k {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, m.artRefreshCmd()

		case "down", "j":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
			return m, m.artRefreshCmd()

		case "enter", "right":
			if len(m.entries) == 0 {
				return m, nil
			}
			if len(m.selected) > 0 {
				cmd := enqueueSelectedCmd(m.selected, m.allEntries)
				m.selected = make(map[string]bool)
				m.status = "Queuing..."
				return m, cmd
			}
			e := m.entries[m.cursor]
			if e.isDir {
				return m, m.navigate(e.path)
			}
			return m, playCmd(e.path, false)

		case "left":
			parent := filepath.Dir(m.dir)
			if parent != m.dir {
				return m, m.navigate(parent)
			}
			return m, nil

		case " ":
			if len(m.entries) == 0 {
				return m, nil
			}
			e := m.entries[m.cursor]
			if m.selected[e.path] {
				delete(m.selected, e.path)
			} else {
				m.selected[e.path] = true
			}
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
			if len(m.selected) == 0 {
				m.status = ""
			} else {
				m.status = fmt.Sprintf("%d selected  Enter queue  Esc clear", len(m.selected))
			}
			return m, nil

		case "backspace", "h":
			parent := filepath.Dir(m.dir)
			if parent != m.dir {
				return m, m.navigate(parent)
			}
			return m, nil

		case "q":
			return m, tea.Quit

		case "/":
			m.filterMode = true
			return m, nil

		case "l":
			if len(m.entries) > 0 {
				e := m.entries[m.cursor]
				if e.isDir {
					return m, m.navigate(e.path)
				}
				return m, playCmd(e.path, false)
			}
			return m, nil

		case "a":
			if len(m.entries) > 0 {
				e := m.entries[m.cursor]
				if e.isDir {
					return m, enqueueFolderCmd(e.path)
				}
				return m, playCmd(e.path, true)
			}
			return m, nil

		case "r":
			if len(m.selected) > 0 {
				return m, playSelectedReplaceCmd(m.selected, m.allEntries)
			}
			if len(m.entries) > 0 {
				e := m.entries[m.cursor]
				if e.isDir {
					return m, playFolderCmd(e.path)
				}
				return m, playCmd(e.path, false)
			}
			return m, nil

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

		case "g":
			m.cursor = 0
			return m, m.artRefreshCmd()

		case "G":
			if len(m.entries) > 0 {
				m.cursor = len(m.entries) - 1
			}
			return m, m.artRefreshCmd()

		case "ctrl+a":
			for _, e := range m.entries {
				m.selected[e.path] = true
			}
			m.status = fmt.Sprintf("%d selected  Enter queue  Esc clear", len(m.selected))
			return m, nil
		}
	}

	return m, nil
}

func (m folderModel) View() string {
	var sb strings.Builder

	w := m.width
	if w < 10 {
		w = 80
	}

	if m.helpMode {
		cfgFile := configPath()
		cfgDir := filepath.Dir(cfgFile)

		sb.WriteString(stylePath.Render("  Folder Browser — Key Bindings") + "\n")
		sb.WriteString(styleDim.Render(strings.Repeat("─", w)) + "\n")
		lines := []string{
			"  Navigation",
			"    ↑↓ / j k       move cursor",
			"    ← / h / Bksp   go up a level",
			"    → / l / Enter   enter folder",
			"    g / G           top / bottom",
			"",
			"  Playback",
			"    Enter           play file (replaces queue)",
			"    r               replace queue with folder / selection",
			"    a               append to queue",
			"    p               pause / resume",
			"    n / N           next / previous track",
			"",
			"  Selection",
			"    Space           toggle mark",
			"    ctrl+a          mark all visible",
			"    Esc             clear marks",
			"",
			"  Filter",
			"    /               enter filter mode",
			"    Esc             exit filter (filter remembered per folder)",
			"",
			"  Other",
			"    i               toggle album art panel",
			"    o               open config.toml in default editor",
			"    O               open config folder",
			"    config.toml     " + terminalLink(cfgFile, cfgFile),
			"    config folder   " + terminalLink(cfgDir, cfgDir),
			"    Tab             switch to Meta browser",
			"    Esc / ? / q     close help",
			"    q / ctrl+c      quit",
		}
		for _, l := range lines {
			sb.WriteString(styleHelp.Render(l) + "\n")
		}
		return sb.String()
	}

	sb.WriteString(stylePath.Render("  "+truncateStr(m.dir, w-2)) + "\n")

	// Show chafa art panel only in mode 1, when fully loaded and terminal wide enough.
	showArt := artMode == 1 && m.artContent != "" && w-artW >= artMinListW
	listW := w
	if showArt {
		listW = w - artW
	}
	sb.WriteString(styleDim.Render(strings.Repeat("─", listW)) + "\n")

	extraLines := 0
	if m.filterMode || m.filter != "" {
		hits := fmt.Sprintf("  %d/%d", len(m.entries), len(m.allEntries))
		prompt := "> " + m.filter
		if m.filterMode {
			prompt += "▌"
		}
		sb.WriteString("  " + styleFilter.Render(prompt) + styleDim.Render(hits) + "\n")
		extraLines = 1
	} else if len(m.selected) > 0 {
		sb.WriteString(styleBadge.Render(fmt.Sprintf("  ● %d selected  Enter queue all  Esc clear", len(m.selected))) + "\n")
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
	if end > len(m.entries) {
		end = len(m.entries)
	}

	for i := start; i < end; i++ {
		e := m.entries[i]
		marked := m.selected[e.path]

		badge := "  "
		if marked {
			badge = styleBadge.Render("● ")
		}

		var label string
		if e.isDir {
			label = "▶ " + e.name + "/"
		} else {
			label = "♪ " + e.name
		}

		var line string
		if i == m.cursor {
			padLen := listW - 2 - 1 - len([]rune(label))
			if padLen < 0 {
				padLen = 0
			}
			line = badge + styleSel.Render(" "+label+strings.Repeat(" ", padLen))
		} else if marked {
			line = badge + styleMarked.Render(label)
		} else if e.isDir {
			line = badge + styleDir.Render(label)
		} else {
			line = badge + styleFile.Render(label)
		}

		sb.WriteString(line + "\n")
	}

	if len(m.entries) == 0 {
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
		if len(m.entries) > 0 {
			pos = fmt.Sprintf("%d/%d", m.cursor+1, len(m.entries))
		}
		if m.filter != "" {
			status = fmt.Sprintf("%d of %d items  %s", len(m.entries), len(m.allEntries), pos)
		} else {
			status = fmt.Sprintf("%d items  %s", len(m.allEntries), pos)
		}
	}
	if m.isErr {
		sb.WriteString(styleErr.Render("  "+status) + "\n")
	} else {
		sb.WriteString(styleStatus.Render("  "+status) + "\n")
	}

	var help string
	artHint := [3]string{"i: art off", "i: chafa▸mpv", "i: mpv▸off"}[artMode]
	if m.filterMode {
		help = "  type to filter (space OK)  \u2191\u2193 navigate  Enter open/play  Backspace delete  Esc exit  Tab\u2192meta"
	} else if len(m.selected) > 0 {
		help = "  Space toggle  Enter queue  a append  ctrl+a all  Esc clear  / filter  Tab\u2192meta  q quit"
	} else {
		help = "  / filter  \u2191\u2193/jk move  \u2190\u2192/hl dir  Enter play  Space select  a append  r replace  n/N next/prev  p pause  " + artHint + "  v view  Tab\u2192meta  q quit"
	}
	sb.WriteString(styleHelp.Render(help))

	if showArt {
		return artJoin(sb.String(), m.artContent)
	}
	return sb.String()
}

// --- folder commands & helpers ---

func navigateCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		entries, err := readDir(dir)
		if err != nil {
			return statusMsg{text: "Error: " + err.Error(), isErr: true}
		}
		return loadedMsg{dir: dir, entries: entries}
	}
}

func enqueueFolderCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		files := collectAudio(dir)
		for _, f := range files {
			mpvLoadFile(pipeName, f, "append-play")
		}
		return statusMsg{text: fmt.Sprintf("Queued %d tracks from %s", len(files), filepath.Base(dir))}
	}
}

func playFolderCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		files := collectAudio(dir)
		if len(files) == 0 {
			return statusMsg{text: "No audio files in " + filepath.Base(dir), isErr: true}
		}
		mpvLoadFile(pipeName, files[0], "replace")
		for _, f := range files[1:] {
			mpvLoadFile(pipeName, f, "append-play")
		}
		return statusMsg{text: fmt.Sprintf("Playing %d tracks from %s", len(files), filepath.Base(dir))}
	}
}

func playSelectedReplaceCmd(selected map[string]bool, allEntries []entry) tea.Cmd {
	snap := make(map[string]bool, len(selected))
	for k, v := range selected {
		snap[k] = v
	}
	entries := make([]entry, len(allEntries))
	copy(entries, allEntries)
	return func() tea.Msg {
		var paths []string
		for _, e := range entries {
			if !snap[e.path] {
				continue
			}
			if e.isDir {
				paths = append(paths, collectAudio(e.path)...)
			} else {
				paths = append(paths, e.path)
			}
		}
		if len(paths) == 0 {
			return statusMsg{text: "Nothing to play", isErr: true}
		}
		mpvLoadFile(pipeName, paths[0], "replace")
		for _, p := range paths[1:] {
			mpvLoadFile(pipeName, p, "append-play")
		}
		return statusMsg{text: fmt.Sprintf("Playing %d tracks", len(paths))}
	}
}

func enqueueSelectedCmd(selected map[string]bool, allEntries []entry) tea.Cmd {
	snap := make(map[string]bool, len(selected))
	for k, v := range selected {
		snap[k] = v
	}
	entries := make([]entry, len(allEntries))
	copy(entries, allEntries)

	return func() tea.Msg {
		count := 0
		for _, e := range entries {
			if !snap[e.path] {
				continue
			}
			if e.isDir {
				for _, f := range collectAudio(e.path) {
					mpvLoadFile(pipeName, f, "append-play")
					count++
				}
			} else {
				mpvLoadFile(pipeName, e.path, "append-play")
				count++
			}
		}
		return statusMsg{text: fmt.Sprintf("Queued %d tracks", count)}
	}
}

func openConfigCmd(openDir bool) tea.Cmd {
	return func() tea.Msg {
		cfgFile := configPath()
		target := cfgFile
		label := "config.toml"
		if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
			_ = writeDefaultConfig(cfgFile)
		}
		if openDir {
			target = filepath.Dir(cfgFile)
			label = "config folder"
		}
		if err := openInDefaultApp(target); err != nil {
			return statusMsg{text: fmt.Sprintf("Open %s failed: %v", label, err), isErr: true}
		}
		return statusMsg{text: fmt.Sprintf("Opened %s", label)}
	}
}

func openInDefaultApp(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func terminalLink(label, target string) string {
	uri := fileURI(target)
	if uri == "" {
		return label
	}
	return "\x1b]8;;" + uri + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

func fileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	u := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(abs)}
	return u.String()
}

func readDir(dir string) ([]entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs, files []entry
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		if de.IsDir() {
			dirs = append(dirs, entry{name: name, isDir: true, path: path})
		} else {
			ext := strings.ToLower(filepath.Ext(name))
			if audioExts[ext] {
				files = append(files, entry{name: name, isDir: false, path: path})
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].name) < strings.ToLower(dirs[j].name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].name) < strings.ToLower(files[j].name)
	})
	return append(dirs, files...), nil
}

func collectAudio(dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if audioExts[ext] {
				files = append(files, path)
			}
		}
		return nil
	})
	sort.Strings(files)
	return files
}
