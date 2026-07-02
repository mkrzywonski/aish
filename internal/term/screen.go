package term

import (
	"strings"
	"sync"

	"github.com/vito/midterm"
)

// Screen wraps the midterm emulator behind a small interface so the
// emulator can be swapped, and adds snapshot/generation semantics so
// readers can detect staleness across resizes.
type Screen struct {
	mu         sync.Mutex
	vt         *midterm.Terminal
	generation uint64
}

type Snapshot struct {
	Text       string `json:"text"`
	CursorRow  int    `json:"cursor_row"`
	CursorCol  int    `json:"cursor_col"`
	Rows       int    `json:"rows"`
	Cols       int    `json:"cols"`
	AltScreen  bool   `json:"alt_screen"`
	Generation uint64 `json:"generation"`
}

func NewScreen(rows, cols int) *Screen {
	return &Screen{vt: midterm.NewTerminal(rows, cols)}
}

func (s *Screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	return s.vt.Write(p)
}

func (s *Screen) Resize(rows, cols int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	s.vt.Resize(rows, cols)
}

// Snapshot returns an atomic rendering of the current screen as plain text,
// with trailing blank space trimmed.
func (s *Screen) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	for _, row := range s.vt.Content {
		b.WriteString(strings.TrimRight(string(row), " "))
		b.WriteByte('\n')
	}
	text := strings.TrimRight(b.String(), "\n") + "\n"

	return Snapshot{
		Text:       text,
		CursorRow:  s.vt.Cursor.Y,
		CursorCol:  s.vt.Cursor.X,
		Rows:       s.vt.Height,
		Cols:       s.vt.Width,
		AltScreen:  s.vt.IsAlt,
		Generation: s.generation,
	}
}
