package selector

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

var (
	ErrCanceled    = errors.New("selection canceled")
	ErrInterrupted = errors.New("selection interrupted")
	ErrNotTTY      = errors.New("interactive selection requires a TTY")
)

type Item struct {
	ID      string
	Display string
	Search  string
}

type match struct {
	index int
	score int
}

type Model struct {
	items    []Item
	query    string
	matches  []match
	cursor   int
	selected map[string]bool
}

func NewModel(items []Item) *Model {
	m := &Model{items: items, selected: make(map[string]bool)}
	m.filter()
	return m
}

func (m *Model) Query() string { return m.query }

func (m *Model) Visible() []Item {
	result := make([]Item, 0, len(m.matches))
	for _, current := range m.matches {
		result = append(result, m.items[current.index])
	}
	return result
}

func (m *Model) Move(delta int) {
	if len(m.matches) == 0 {
		m.cursor = 0
		return
	}
	m.cursor = (m.cursor + delta) % len(m.matches)
	if m.cursor < 0 {
		m.cursor += len(m.matches)
	}
}

func (m *Model) Toggle() {
	if len(m.matches) == 0 {
		return
	}
	id := m.items[m.matches[m.cursor].index].ID
	m.selected[id] = !m.selected[id]
}

func (m *Model) Append(text string) {
	m.query += text
	m.cursor = 0
	m.filter()
}

func (m *Model) Backspace() {
	if m.query == "" {
		return
	}
	_, size := utf8.DecodeLastRuneInString(m.query)
	m.query = m.query[:len(m.query)-size]
	m.cursor = 0
	m.filter()
}

func (m *Model) Selected() []Item {
	result := make([]Item, 0, len(m.selected))
	// Preserve source/topological order, never fuzzy ranking order.
	for _, item := range m.items {
		if m.selected[item.ID] {
			result = append(result, item)
		}
	}
	return result
}

func (m *Model) filter() {
	m.matches = m.matches[:0]
	for i, item := range m.items {
		score, ok := fuzzyScore(m.query, item.Search)
		if ok {
			m.matches = append(m.matches, match{index: i, score: score})
		}
	}
	if m.query != "" {
		sort.SliceStable(m.matches, func(i, j int) bool { return m.matches[i].score > m.matches[j].score })
	}
	if m.cursor >= len(m.matches) {
		m.cursor = max(0, len(m.matches)-1)
	}
}

func fuzzyScore(query, candidate string) (int, bool) {
	query = strings.ToLower(query)
	candidate = strings.ToLower(candidate)
	if query == "" {
		return 0, true
	}
	q := []rune(query)
	c := []rune(candidate)
	qi, score, consecutive := 0, 0, 0
	for i, r := range c {
		if qi >= len(q) || r != q[qi] {
			consecutive = 0
			continue
		}
		consecutive++
		score += 10 + consecutive*4
		if i == 0 || c[i-1] == ' ' || c[i-1] == '-' || c[i-1] == '/' {
			score += 8
		}
		qi++
	}
	if qi != len(q) {
		return 0, false
	}
	return score - len(c), true
}

type Terminal struct {
	In  *os.File
	Out *os.File
}

func (t Terminal) Select(items []Item, title string) ([]Item, error) {
	if t.In == nil || t.Out == nil || !term.IsTerminal(int(t.In.Fd())) || !term.IsTerminal(int(t.Out.Fd())) {
		return nil, ErrNotTTY
	}
	oldState, err := term.MakeRaw(int(t.In.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enable terminal raw mode: %w", err)
	}
	fmt.Fprint(t.Out, "\x1b[?1049h\x1b[?25l")
	defer func() {
		fmt.Fprint(t.Out, "\x1b[?25h\x1b[?1049l")
		_ = term.Restore(int(t.In.Fd()), oldState)
	}()

	model := NewModel(items)
	buffer := make([]byte, 64)
	for {
		render(t.Out, model, title)
		n, readErr := t.In.Read(buffer)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, ErrCanceled
			}
			return nil, readErr
		}
		key := buffer[:n]
		switch {
		case len(key) == 1 && key[0] == 3:
			return nil, ErrInterrupted
		case len(key) == 1 && key[0] == 27:
			return nil, ErrCanceled
		case string(key) == "\x1b[A":
			model.Move(-1)
		case string(key) == "\x1b[B":
			model.Move(1)
		case len(key) == 1 && (key[0] == '\r' || key[0] == '\n'):
			return model.Selected(), nil
		case len(key) == 1 && key[0] == ' ':
			model.Toggle()
		case len(key) == 1 && (key[0] == 127 || key[0] == 8):
			model.Backspace()
		case len(key) == 1 && model.Query() == "" && key[0] == 'j':
			model.Move(1)
		case len(key) == 1 && model.Query() == "" && key[0] == 'k':
			model.Move(-1)
		default:
			if text := printable(key); text != "" {
				model.Append(text)
			}
		}
	}
}

func printable(value []byte) string {
	var result strings.Builder
	for len(value) > 0 {
		r, size := utf8.DecodeRune(value)
		if r == utf8.RuneError && size == 1 {
			value = value[1:]
			continue
		}
		if r >= 0x20 && r != 0x7f {
			result.WriteRune(r)
		}
		value = value[size:]
	}
	return result.String()
}

func render(out *os.File, model *Model, title string) {
	_, height, err := term.GetSize(int(out.Fd()))
	if err != nil || height < 10 {
		height = 24
	}
	rows := height - 7
	start := 0
	if model.cursor >= rows {
		start = model.cursor - rows + 1
	}
	end := min(len(model.matches), start+rows)

	fmt.Fprint(out, "\x1b[H\x1b[2J")
	fmt.Fprintf(out, "\x1b[1m%s\x1b[0m\r\n", title)
	fmt.Fprintf(out, "검색: %s\r\n", model.query)
	fmt.Fprint(out, "SPACE 선택 · ENTER 확정 · ESC 취소 · ↑/↓ 이동\r\n\r\n")
	for row := start; row < end; row++ {
		item := model.items[model.matches[row].index]
		pointer := "  "
		if row == model.cursor {
			pointer = "› "
		}
		marker := "  "
		if model.selected[item.ID] {
			marker = "✓ "
		}
		fmt.Fprintf(out, "%s%s%s\x1b[K\r\n", pointer, marker, item.Display)
	}
	if len(model.matches) == 0 {
		fmt.Fprint(out, "  일치하는 커밋이 없습니다.\r\n")
	}
	fmt.Fprintf(out, "\r\n%d개 표시 · %d개 선택\x1b[K", len(model.matches), len(model.Selected()))
}
