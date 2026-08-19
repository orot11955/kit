package selector

import (
	"errors"
	"os"
	"testing"
)

func TestModelFuzzyFilterAndSelectionKeepsSourceOrder(t *testing.T) {
	items := []Item{
		{ID: "a", Search: "abc add login", Display: "first"},
		{ID: "b", Search: "def update docs", Display: "second"},
		{ID: "c", Search: "ghi login test", Display: "third"},
	}
	m := NewModel(items)
	m.Append("log")
	visible := m.Visible()
	if len(visible) != 2 || visible[0].ID != "a" || visible[1].ID != "c" {
		t.Fatalf("unexpected fuzzy matches: %#v", visible)
	}
	m.Move(1)
	m.Toggle() // c
	m.Move(-1)
	m.Toggle() // a
	selected := m.Selected()
	if len(selected) != 2 || selected[0].ID != "a" || selected[1].ID != "c" {
		t.Fatalf("selection lost source order: %#v", selected)
	}
}

func TestModelBackspaceAndWrap(t *testing.T) {
	m := NewModel([]Item{{ID: "a", Search: "alpha"}, {ID: "b", Search: "beta"}})
	m.Move(-1)
	if got := m.Visible()[m.cursor].ID; got != "b" {
		t.Fatalf("cursor did not wrap: %s", got)
	}
	m.Append("β")
	if m.Query() != "β" || len(m.Visible()) != 0 {
		t.Fatalf("unexpected query state: %q %#v", m.Query(), m.Visible())
	}
	m.Backspace()
	if m.Query() != "" || len(m.Visible()) != 2 {
		t.Fatalf("unicode backspace failed: %q", m.Query())
	}
}

func TestTerminalRejectsNonTTY(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	_, err = (Terminal{In: input, Out: output}).Select(nil, "test")
	if !errors.Is(err, ErrNotTTY) {
		t.Fatalf("expected ErrNotTTY, got %v", err)
	}
}
