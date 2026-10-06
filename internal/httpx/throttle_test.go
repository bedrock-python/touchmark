package httpx

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/throttle"
)

// The throttle the request's context carries sees every request before it
// is sent: its reads and writes are paced, a
// request it refuses is not sent, and an answer that shows the budget
// spent pauses the provider.
func TestThrottle(t *testing.T) {
	var sent []string
	s := newServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Method)
		if r.Method == http.MethodPost {
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", "30")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var slept []time.Duration
	g := throttle.New(throttle.Options{Name: "gl", Limits: throttle.Limits{MinInterval: time.Second}, Clock: throttle.Clock{
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}})
	ctx := throttle.With(t.Context(), g)
	c := New(Options{})
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodPost, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, s.URL+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Do(req, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The PATCH and the POST are writes a second apart; the POST's answer
	// spent the budget, so the last GET waited for its reset.
	if !slices.Equal(slept, []time.Duration{time.Second, 30 * time.Second}) {
		t.Errorf("waits %v", slept)
	}
	if st := g.Stats(); st.Writes != 2 || st.Reads != 2 {
		t.Errorf("stats %+v", st)
	}
	// A provider out of budget: nothing is sent.
	for range throttle.Strikes {
		g.Limited(g.Ticket(throttle.WriteCall), 0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(req, nil)
	if e, ok := throttle.Refused(err); !ok || e.Reason != throttle.ReasonRateLimit {
		t.Errorf("a request to a provider out of budget: %v", err)
	}
	if len(sent) != 4 {
		t.Errorf("sent %v", sent)
	}
}
