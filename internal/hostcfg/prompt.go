package hostcfg

import (
	"io"

	"github.com/charmbracelet/huh"
)

type huhPrompter struct {
	in  io.Reader
	out io.Writer
}

// NewHuhPrompter returns an interactive prompter on in/out (needs a TTY).
func NewHuhPrompter(in io.Reader, out io.Writer) Prompter {
	return &huhPrompter{in: in, out: out}
}

func (h *huhPrompter) Ask(q Question, def string) (string, error) {
	val := def
	var field huh.Field
	switch {
	case len(q.Options) > 0:
		field = huh.NewSelect[string]().Title(q.Prompt).Description(q.Help).
			Options(huh.NewOptions(q.Options...)...).Value(&val)
	default:
		in := huh.NewInput().Title(q.Prompt).Description(q.Help).Value(&val)
		if q.Secret {
			in = in.EchoMode(huh.EchoModePassword)
		}
		if q.Validate != nil {
			validate := q.Validate
			in = in.Validate(func(s string) error {
				if s == "" && q.Optional {
					return nil
				}
				return validate(s)
			})
		}
		field = in
	}
	err := huh.NewForm(huh.NewGroup(field)).WithInput(h.in).WithOutput(h.out).Run()
	return val, err
}
