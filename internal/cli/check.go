package cli

import (
	"context"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/report"
)

// checkRun collects the findings of check.
type checkRun struct {
	hub *hub
	rep *report.Check
}

func (c *checkRun) addErrors(msgs ...string)   { c.rep.Errors = append(c.rep.Errors, msgs...) }
func (c *checkRun) addWarnings(msgs ...string) { c.rep.Warnings = append(c.rep.Warnings, msgs...) }

// runCheck validates hub.yml, targets.yml, .touchmark/operations.yml, the
// write isolation probe of the GitHub Actions workflows, the history and
// every pack. It reports every finding it can instead of stopping at the
// first: config files first, then the probe, then the history, then the
// packs. Operations past their date are warnings. It exits 2 when there is
// an error.
func runCheck(ctx context.Context, e *env, o *options) error {
	h, err := openHub(ctx, e, o.hub, o.worktree)
	if err != nil {
		return err
	}
	c := &checkRun{hub: h, rep: &report.Check{
		Command:  "check",
		Packs:    []string{},
		Errors:   []string{},
		Warnings: []string{},
	}}
	for _, err := range h.readConfigs(ctx) {
		c.addErrors(flatten(err)...)
	}
	if err := h.readOperations(ctx); err != nil {
		c.addErrors(flatten(err)...)
	}
	c.addWarnings(h.warnings...)
	c.rep.Hub = h.report()
	if err := h.readPacks(ctx); err != nil {
		return err
	}
	c.rep.Packs = h.packNames()
	c.crossFile()
	if err := c.codeowners(ctx); err != nil {
		return err
	}
	if err := c.probe(ctx); err != nil {
		return err
	}
	if err := c.history(ctx); err != nil {
		return err
	}
	c.packs()
	if err := c.print(e, o.format); err != nil {
		return err
	}
	if len(c.rep.Errors) > 0 {
		return exitWith(exitUsage)
	}
	return nil
}

// crossFile runs the checks that need both config files and the pack list,
// then those of operations.yml against them; they are skipped when a file
// failed to parse.
func (c *checkRun) crossFile() {
	h := c.hub
	if h.cfg == nil || h.targets == nil {
		return
	}
	warns, errs := config.Check(h.cfg, h.targets, h.known())
	if h.ops != nil {
		opWarns, opErrs := config.CheckOperations(h.ops, h.targets, h.cfg, time.Now())
		warns, errs = append(warns, opWarns...), append(errs, opErrs...)
	}
	for _, w := range warns {
		c.addWarnings(w.String())
	}
	for _, err := range errs {
		c.addErrors(flatten(err)...)
	}
}

// codeowners warns about a CODEOWNERS file of the hub that still names the
// template's placeholder owner (config.CheckCodeowners). Skipped when
// hub.yml failed to parse.
func (c *checkRun) codeowners(ctx context.Context) error {
	if c.hub.cfg == nil {
		return nil
	}
	files := map[string][]byte{}
	for _, name := range config.CodeownersFiles {
		data, err := c.hub.readConfig(ctx, name)
		if err != nil {
			return err
		}
		if data != nil {
			files[name] = data
		}
	}
	for _, w := range config.CheckCodeowners(c.hub.cfg, files) {
		c.addWarnings(w.String())
	}
	return nil
}

// probe checks the write isolation probe of the hub's GitHub Actions
// workflows (config.CheckProbe): every secret a workflow
// hands touchmark as a write key (an env variable or an action input) must
// be tested by the probe, and the write key of every provider of hub.yml
// must be handed out where the check can see it. Skipped when hub.yml
// failed to parse.
func (c *checkRun) probe(ctx context.Context) error {
	if c.hub.cfg == nil {
		return nil
	}
	files, err := c.hub.workflows(ctx)
	if err != nil {
		return err
	}
	for _, err := range config.CheckProbe(c.hub.cfg, files) {
		c.addErrors(err.Error())
	}
	return nil
}

// history builds the manifest: a shallow or partial hub is an error,
// historical entries that fail today's rules are warnings.
func (c *checkRun) history(ctx context.Context) error {
	err := c.hub.build(ctx)
	if isGuard(err) {
		c.addErrors(err.Error())
		return nil
	}
	if err != nil {
		return err
	}
	for _, p := range c.hub.skipped {
		c.addWarnings(display(p) + ": ignored in history: not a valid pack file path")
	}
	return nil
}

// packs reports the defects of what the packs ship now, sorted by pack,
// path and message.
func (c *checkRun) packs() {
	for _, p := range c.hub.packProblems() {
		c.addErrors(p.String())
	}
}

func (c *checkRun) print(e *env, format string) error {
	if format == formatJSON {
		return report.WriteJSON(e.stdout, c.rep)
	}
	return c.rep.WriteText(e.stdout)
}
