package distribute

import (
	"cmp"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// limitsOf returns a provider's pacing: the platform's defaults (Caps.Limits
// from its Probe), each overridden by providers[].limits in hub.yml where
// that sets it (a non-zero number, a min_interval). The provider's targets
// inspected at once never exceed the run's concurrency (each provider has a
// pool of its own, eachByProvider).
func limitsOf(caps platform.Limits, cfg config.ProviderLimits, concurrency int) throttle.Limits {
	l := throttle.Limits{
		Inspections:       cmp.Or(cfg.Reads, caps.Reads),
		GitReads:          cmp.Or(cfg.GitReads, caps.GitReads),
		ReadsPerMinute:    cmp.Or(cfg.ReadsPerMinute, caps.ReadsPerMinute),
		WritesPerMinute:   cmp.Or(cfg.WritesPerMinute, caps.WritesPerMinute),
		WritesPerHour:     cmp.Or(cfg.WritesPerHour, caps.WritesPerHour),
		CommentsPerMinute: cmp.Or(cfg.CommentsPerMinute, caps.CommentsPerMinute),
		MinInterval:       cmp.Or(cfg.Interval(), caps.MinInterval),
	}
	if n := cmp.Or(concurrency, defaultConcurrency); l.Inspections <= 0 || l.Inspections > n {
		l.Inspections = n
	}
	return l
}
