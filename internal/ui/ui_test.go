package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestRendererKeepsMeaningWithoutColor(t *testing.T) {
	var output bytes.Buffer
	renderer := Renderer{Writer: &output}
	renderer.Command("review submit")
	renderer.Success("Push", "origin/feat/login")
	renderer.Warning("Sync", "필요")
	renderer.Next("kit git review finish")

	got := output.String()
	for _, want := range []string{"kit · review submit", "✓ Push", "! Sync", "$ kit git review finish"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain renderer emitted ANSI: %q", got)
	}
}

func TestRendererUsesANSIOnlyWhenEnabled(t *testing.T) {
	var output bytes.Buffer
	Renderer{Writer: &output, Color: true}.Notice("머지 완료")
	if !strings.Contains(output.String(), "\x1b[") {
		t.Fatalf("color renderer did not emit ANSI: %q", output.String())
	}
}

func TestRendererSanitizesControlCharacters(t *testing.T) {
	var output bytes.Buffer
	renderer := Renderer{Writer: &output}
	renderer.Field("Title", "safe\x1b]8;;https://evil.invalid\a link\nnext")
	got := output.String()
	if strings.ContainsAny(got, "\x1b\a") || strings.Count(got, "\n") != 1 {
		t.Fatalf("control character reached terminal output: %q", got)
	}
}

func TestShellQuoteProtectsCopyableCommands(t *testing.T) {
	if got := ShellQuote("feat/login"); got != "feat/login" {
		t.Fatalf("safe branch was unnecessarily changed: %q", got)
	}
	if got := ShellQuote("feat;echo-pwn"); got != "'feat;echo-pwn'" {
		t.Fatalf("unsafe branch was not quoted: %q", got)
	}
	if got := ShellQuote("feat'quote"); got != "'feat'\"'\"'quote'" {
		t.Fatalf("single quote was not escaped: %q", got)
	}
}
