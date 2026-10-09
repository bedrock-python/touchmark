package azuree2e

import (
	"fmt"
	"strings"
	"testing"
)

// evaluate evaluates a stage condition of Azure Pipelines in the subset the
// template uses (learn.microsoft.com/en-us/azure/devops/pipelines/process/expressions):
// and, or, not, eq, ne, in, succeeded(), 'string' literals and
// variables['Name'], read from env as the agent exports it (uppercased, '.'
// as '_'; unset is ""). Strings compare ignoring case, as Azure's do.
// succeeded is what succeeded() returns.
func evaluate(cond string, env map[string]string, succeeded bool) (bool, error) {
	p := &parser{s: strings.TrimSpace(cond), env: env, succeeded: succeeded}
	v, err := p.value()
	if err != nil {
		return false, err
	}
	p.space()
	if p.i != len(p.s) {
		return false, fmt.Errorf("trailing %q", p.s[p.i:])
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("the condition is %q, not a boolean", v)
	}
	return b, nil
}

type parser struct {
	s         string
	i         int
	env       map[string]string
	succeeded bool
}

func (p *parser) space() {
	for p.i < len(p.s) && strings.ContainsRune(" \t\r\n", rune(p.s[p.i])) {
		p.i++
	}
}

func (p *parser) eat(c byte) error {
	p.space()
	if p.i >= len(p.s) || p.s[p.i] != c {
		return fmt.Errorf("want %q at %d of %q", c, p.i, p.s)
	}
	p.i++
	return nil
}

// value parses one value: a literal, a variable or a function call.
func (p *parser) value() (any, error) {
	p.space()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("unexpected end of %q", p.s)
	}
	if p.s[p.i] == '\'' {
		end := strings.IndexByte(p.s[p.i+1:], '\'')
		if end < 0 {
			return nil, fmt.Errorf("unterminated string in %q", p.s)
		}
		v := p.s[p.i+1 : p.i+1+end]
		p.i += end + 2
		return v, nil
	}
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] >= 'a' && p.s[p.i] <= 'z' || p.s[p.i] >= 'A' && p.s[p.i] <= 'Z') {
		p.i++
	}
	name := p.s[start:p.i]
	if name == "variables" {
		if err := p.eat('['); err != nil {
			return nil, err
		}
		key, err := p.value()
		if err != nil {
			return nil, err
		}
		if err := p.eat(']'); err != nil {
			return nil, err
		}
		return p.env[strings.ToUpper(strings.ReplaceAll(fmt.Sprint(key), ".", "_"))], nil
	}
	if err := p.eat('('); err != nil {
		return nil, err
	}
	var args []any
	p.space()
	if p.i < len(p.s) && p.s[p.i] == ')' {
		p.i++
	} else {
		for {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			args = append(args, v)
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if err := p.eat(')'); err != nil {
				return nil, err
			}
			break
		}
	}
	return apply(name, args, p.succeeded)
}

// apply applies function name to args.
func apply(name string, args []any, succeeded bool) (any, error) {
	bools := func() ([]bool, error) {
		var out []bool
		for _, a := range args {
			b, ok := a.(bool)
			if !ok {
				return nil, fmt.Errorf("%s(%q): not a boolean", name, a)
			}
			out = append(out, b)
		}
		return out, nil
	}
	same := func(a, b any) bool { return strings.EqualFold(fmt.Sprint(a), fmt.Sprint(b)) }
	switch name {
	case "succeeded":
		return succeeded, nil
	case "and", "or":
		bs, err := bools()
		if err != nil {
			return nil, err
		}
		if len(bs) < 2 {
			return nil, fmt.Errorf("%s: %d arguments", name, len(bs))
		}
		for _, b := range bs {
			if b == (name == "or") {
				return b, nil
			}
		}
		return name == "and", nil
	case "not":
		bs, err := bools()
		if err != nil {
			return nil, err
		}
		if len(bs) != 1 {
			return nil, fmt.Errorf("not: %d arguments", len(bs))
		}
		return !bs[0], nil
	case "eq", "ne":
		if len(args) != 2 {
			return nil, fmt.Errorf("%s: %d arguments", name, len(args))
		}
		return same(args[0], args[1]) == (name == "eq"), nil
	case "in":
		if len(args) < 2 {
			return nil, fmt.Errorf("in: %d arguments", len(args))
		}
		for _, a := range args[1:] {
			if same(args[0], a) {
				return true, nil
			}
		}
		return false, nil
	}
	return nil, fmt.Errorf("unknown function %q", name)
}

func TestEvaluate(t *testing.T) {
	env := map[string]string{"BUILD_REASON": "Manual", "BUILD_SOURCEBRANCH": "refs/heads/main"}
	for cond, want := range map[string]bool{
		"succeeded()": true,
		"and(succeeded(), eq(variables['Build.Reason'], 'manual'))":                      true,
		"or(eq(variables['Build.Reason'], 'Schedule'), ne('a', 'A'))":                    false,
		"in(variables['Build.SourceBranch'], 'refs/heads/master', 'refs/heads/main')":    true,
		"not(in(variables['Build.SourceBranch'], 'refs/heads/master'))":                  true,
		"eq(variables['Build.CronSchedule.DisplayName'], '')":                            true,
		"and(succeeded(),\n  or(eq('x', 'y'), eq(variables['Build.Reason'], 'Manual')))": true,
	} {
		got, err := evaluate(cond, env, true)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", cond, got, err, want)
		}
	}
	for _, bad := range []string{"eq('a')", "and(true)", "'a'", "eq('a', 'b') extra", "frob()"} {
		if _, err := evaluate(bad, env, true); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
