// Package hostcfgtest provides a scripted hostcfg.Prompter for tests.
package hostcfgtest

import (
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
)

// Prompter answers by Question.Key and fails the test on any key it was not
// given, so a test notices when code asks something unexpected.
type Prompter struct {
	t       testing.TB
	answers map[string]string
	Asked   []string
}

func NewPrompter(t testing.TB, answers map[string]string) *Prompter {
	return &Prompter{t: t, answers: answers}
}

func (p *Prompter) Ask(q hostcfg.Question, def string) (string, error) {
	p.t.Helper()
	p.Asked = append(p.Asked, q.Key)
	a, ok := p.answers[q.Key]
	if !ok {
		p.t.Fatalf("unexpected question %q", q.Key)
	}
	return a, nil
}
