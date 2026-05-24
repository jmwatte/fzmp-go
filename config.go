package main

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// layoutConfig is one named drill-down path defined in the config file.
type layoutConfig struct {
	Name   string   `toml:"name"`
	Levels []string `toml:"levels"`
}

// appConfig holds all user-configurable settings.
type appConfig struct {
	MusicRoot  string `toml:"music_root"`
	PipeName   string `toml:"pipe_name"`
	NoArtImage string `toml:"no_art_image"` // placeholder shown when no album art exists
	MpvPath    string `toml:"mpv_path"`     // path to mpv executable (default: "mpv")
	ChafaPath  string `toml:"chafa_path"`   // path to chafa executable (default: "chafa")
	FfmpegPath string `toml:"ffmpeg_path"`  // path to ffmpeg executable (default: "ffmpeg")

	Meta struct {
		CSVPath    string         `toml:"csv_path"`
		PathColumn string         `toml:"path_column"` // override auto-detection
		Layouts    []layoutConfig `toml:"layouts"`
	} `toml:"meta"`
}

func defaultConfig() appConfig {
	var cfg appConfig
	cfg.MusicRoot = `H:\music`
	cfg.PipeName = "fzmp"
	cfg.MpvPath = "mpv"
	cfg.ChafaPath = "chafa"
	cfg.FfmpegPath = "ffmpeg"
	cfg.Meta.CSVPath = ""
	cfg.Meta.Layouts = []layoutConfig{
		{Name: "Genre › Artist › Album", Levels: []string{"Genre", "Album Artist", "Album"}},
		{Name: "Artist › Album", Levels: []string{"Album Artist", "Album"}},
		{Name: "Year › Artist › Album", Levels: []string{"Year", "Album Artist", "Album"}},
	}
	// Auto-detect NoArt placeholder: check PS fzmp module dir, then exe dir.
	for _, candidate := range []string{
		filepath.Join(os.Getenv("USERPROFILE"), `Documents\PowerShell\Modules\fzmp\NoArt.png`),
		filepath.Join(os.Getenv("APPDATA"), `fzmp\NoArt.png`),
	} {
		if _, err := os.Stat(candidate); err == nil {
			cfg.NoArtImage = candidate
			break
		}
	}
	return cfg
}

// configPath returns %APPDATA%\fzmp\config.toml
func configPath() string {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		appdata, _ = os.UserConfigDir()
	}
	return filepath.Join(appdata, "fzmp", "config.toml")
}

// loadConfig reads config.toml, falling back to defaults for missing fields.
func loadConfig() (appConfig, error) {
	cfg := defaultConfig()
	path := configPath()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil // no config file — use defaults silently
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, err
	}

	// If layouts were specified in TOML, replace the defaults entirely.
	// (partial merge is confusing; explicit > implicit)

	// Fill in any empty required fields
	if cfg.PipeName == "" {
		cfg.PipeName = "fzmp"
	}

	return cfg, nil
}

// writeDefaultConfig creates a commented example config at the standard path.
func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	const example = `# fzmp-go configuration
# Location: %APPDATA%\fzmp\config.toml

# Root directory shown in the folder browser
music_root = 'H:\music'

# mpv named-pipe name (must match mpv --input-ipc-server=\\.\pipe\NAME)
pipe_name = "fzmp"

[meta]
# Path to your foobar2000 CSV export
csv_path = ''

# Override auto-detection of the file-path column ("Full Path", "Path", "Filename")
# path_column = "Full Path"

# Drill-down layouts — list them in order of preference.
# L / H keys cycle through them while in the meta browser.
# levels must be exact CSV column header names.

[[meta.layouts]]
name   = "Genre › Artist › Album"
levels = ["Genre", "Album Artist", "Album"]

[[meta.layouts]]
name   = "Artist › Album"
levels = ["Album Artist", "Album"]

[[meta.layouts]]
name   = "Year › Artist › Album"
levels = ["Year", "Album Artist", "Album"]
`
	return os.WriteFile(path, []byte(example), 0644)
}
