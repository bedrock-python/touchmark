package cli

import (
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/redact"
)

// lockedBuilder is a strings.Builder safe for concurrent writes.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMaskNewSecrets: on GitHub Actions every secret the drivers register
// after the start (a JWT, an installation token, a per-target token) gets
// its "::add-mask::" commands before the registration returns, whole
// lines even when the drivers mint concurrently.
func TestMaskNewSecrets(t *testing.T) {
	t.Parallel()
	reg := redact.New()
	reg.Add("known-before-start")
	var out lockedBuilder
	maskNewSecrets(reg, &out)
	if out.String() != "" {
		t.Errorf("wrote %q before any new secret", out.String())
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.Add("ghs_minted_token_"+strings.Repeat("x", i), "x-access-token")
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 40 {
		t.Errorf("%d lines, want a raw and a Basic form of 20 tokens", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "::add-mask::") || strings.Contains(l, "known-before-start") {
			t.Errorf("line %q", l)
		}
	}
	reg.Add("ghs_minted_token_" + strings.Repeat("x", 3))
	if n := strings.Count(out.String(), "\n"); n != 40 {
		t.Errorf("a secret registered again printed more: %d lines", n)
	}
}
