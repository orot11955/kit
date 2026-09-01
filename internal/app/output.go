package app

import (
	"encoding/json"
	"io"
	"os"

	"golang.org/x/term"

	"kit/internal/clierror"
	"kit/internal/ui"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return clierror.Wrap(clierror.Failure, err, "write JSON output")
	}
	return nil
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (a *Application) renderer(global globalOptions) ui.Renderer {
	color := !global.noColor && os.Getenv("NO_COLOR") == "" && isTerminal(a.IO.Out)
	return ui.Renderer{Writer: a.IO.Out, Color: color}
}
