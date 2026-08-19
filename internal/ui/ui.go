package ui

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

type Renderer struct {
	Writer io.Writer
	Color  bool
}

func (r Renderer) Command(name string) {
	fmt.Fprintf(r.Writer, "%skit · %s%s\n\n", r.code("\x1b[1;36m"), SafeText(name), r.reset())
}

func (r Renderer) Notice(title string) {
	fmt.Fprintf(r.Writer, "%skit ! %s%s\n\n", r.code("\x1b[1;33m"), SafeText(title), r.reset())
}

func (r Renderer) Success(label, value string) {
	fmt.Fprintf(r.Writer, "  %s✓%s %-12s %s\n", r.code("\x1b[32m"), r.reset(), SafeText(label), SafeText(value))
}

func (r Renderer) Warning(label, value string) {
	fmt.Fprintf(r.Writer, "  %s!%s %-12s %s\n", r.code("\x1b[33m"), r.reset(), SafeText(label), SafeText(value))
}

func (r Renderer) Field(label, value string) {
	fmt.Fprintf(r.Writer, "    %-12s %s\n", SafeText(label), SafeText(value))
}

func (r Renderer) Next(command string) {
	fmt.Fprintf(r.Writer, "\n다음\n  %s$ %s%s\n", r.code("\x1b[36m"), SafeText(command), r.reset())
}

func ShellQuote(argument string) string {
	if argument != "" && strings.IndexFunc(argument, func(character rune) bool {
		return !(unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("_@%+=:,./-", character))
	}) == -1 {
		return argument
	}
	return "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
}

func SafeText(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
}

func (r Renderer) code(value string) string {
	if !r.Color {
		return ""
	}
	return value
}

func (r Renderer) reset() string {
	if !r.Color {
		return ""
	}
	return "\x1b[0m"
}
