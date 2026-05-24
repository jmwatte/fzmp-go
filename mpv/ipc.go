package mpv

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Microsoft/go-winio"
)

// LoadFile sends a loadfile command to mpv via Windows named pipe.
// mode: "replace", "append", "append-play"
func LoadFile(pipeName, path, mode string) error {
	return send(pipeName, []any{"loadfile", path, mode})
}

// Pause toggles pause.
func Pause(pipeName string) error {
	return send(pipeName, []any{"cycle", "pause"})
}

// Next skips to the next playlist entry.
func Next(pipeName string) error {
	return send(pipeName, []any{"playlist-next", "weak"})
}

// Prev skips to the previous playlist entry.
func Prev(pipeName string) error {
	return send(pipeName, []any{"playlist-prev", "weak"})
}

func send(pipeName string, args []any) error {
	pipePath := `\\.\pipe\` + pipeName
	timeout := 2 * time.Second

	conn, err := winio.DialPipe(pipePath, &timeout)
	if err != nil {
		return fmt.Errorf("mpv pipe %q: %w", pipePath, err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))

	payload := map[string]any{"command": args}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// mpv requires LF line ending (not CRLF)
	data = append(data, '\n')

	_, err = conn.Write(data)
	return err
}
