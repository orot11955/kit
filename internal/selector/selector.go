package selector

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
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

type terminalKeyKind uint8

const (
	terminalKeyUnknown terminalKeyKind = iota
	terminalKeyInterrupt
	terminalKeyEscape
	terminalKeyUp
	terminalKeyDown
	terminalKeyEnter
	terminalKeySpace
	terminalKeyBackspace
	terminalKeyText
)

const escapeSequenceWaitMS = 50

type terminalKey struct {
	kind terminalKeyKind
	text string
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
	for {
		render(t.Out, model, title)
		key, readErr := readTerminalKey(t.In)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, ErrCanceled
			}
			return nil, readErr
		}
		switch key.kind {
		case terminalKeyInterrupt:
			return nil, ErrInterrupted
		case terminalKeyEscape:
			return nil, ErrCanceled
		case terminalKeyUp:
			model.Move(-1)
		case terminalKeyDown:
			model.Move(1)
		case terminalKeyEnter:
			return model.Selected(), nil
		case terminalKeySpace:
			model.Toggle()
		case terminalKeyBackspace:
			model.Backspace()
		case terminalKeyText:
			if model.Query() == "" && key.text == "j" {
				model.Move(1)
			} else if model.Query() == "" && key.text == "k" {
				model.Move(-1)
			} else {
				model.Append(key.text)
			}
		}
	}
}

func readTerminalKey(input *os.File) (terminalKey, error) {
	first, err := readByte(input)
	if err != nil {
		return terminalKey{}, err
	}
	switch first {
	case 3:
		return terminalKey{kind: terminalKeyInterrupt}, nil
	case 27:
		return readEscapeKey(input)
	case '\r', '\n':
		return terminalKey{kind: terminalKeyEnter}, nil
	case ' ':
		return terminalKey{kind: terminalKeySpace}, nil
	case 127, 8:
		return terminalKey{kind: terminalKeyBackspace}, nil
	}
	if first >= 0x20 && first < utf8.RuneSelf {
		return terminalKey{kind: terminalKeyText, text: string([]byte{first})}, nil
	}

	length := utf8SequenceLength(first)
	if length == 0 {
		return terminalKey{kind: terminalKeyUnknown}, nil
	}
	value := make([]byte, length)
	value[0] = first
	for i := 1; i < length; i++ {
		value[i], err = readByte(input)
		if err != nil {
			return terminalKey{}, err
		}
	}
	r, size := utf8.DecodeRune(value)
	if size != len(value) || (r == utf8.RuneError && size == 1) || r < 0x20 || r == 0x7f {
		return terminalKey{kind: terminalKeyUnknown}, nil
	}
	return terminalKey{kind: terminalKeyText, text: string(r)}, nil
}

func readEscapeKey(input *os.File) (terminalKey, error) {
	ready, err := waitForTerminalInput(input)
	if err != nil {
		return terminalKey{}, err
	}
	if !ready {
		return terminalKey{kind: terminalKeyEscape}, nil
	}
	second, err := readByte(input)
	if errors.Is(err, io.EOF) {
		return terminalKey{kind: terminalKeyEscape}, nil
	}
	if err != nil {
		return terminalKey{}, err
	}
	if second != '[' && second != 'O' {
		return terminalKey{kind: terminalKeyUnknown}, nil
	}
	ready, err = waitForTerminalInput(input)
	if err != nil {
		return terminalKey{}, err
	}
	if !ready {
		return terminalKey{kind: terminalKeyUnknown}, nil
	}
	third, err := readByte(input)
	if err != nil {
		return terminalKey{}, err
	}
	switch third {
	case 'A':
		return terminalKey{kind: terminalKeyUp}, nil
	case 'B':
		return terminalKey{kind: terminalKeyDown}, nil
	default:
		return terminalKey{kind: terminalKeyUnknown}, nil
	}
}

func waitForTerminalInput(input *os.File) (bool, error) {
	poll := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
	for {
		count, err := unix.Poll(poll, escapeSequenceWaitMS)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("poll terminal input: %w", err)
		}
		if count == 0 {
			return false, nil
		}
		events := poll[0].Revents
		if events&unix.POLLNVAL != 0 {
			return false, errors.New("terminal input descriptor is invalid")
		}
		return events&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0, nil
	}
}

func readByte(input *os.File) (byte, error) {
	var value [1]byte
	for {
		n, err := input.Read(value[:])
		if n == 1 {
			return value[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func utf8SequenceLength(first byte) int {
	switch {
	case first >= 0xc2 && first <= 0xdf:
		return 2
	case first >= 0xe0 && first <= 0xef:
		return 3
	case first >= 0xf0 && first <= 0xf4:
		return 4
	default:
		return 0
	}
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
