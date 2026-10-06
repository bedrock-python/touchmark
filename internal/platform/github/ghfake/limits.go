package ghfake

import (
	"net/http"
	"strconv"
	"time"
)

// Limits are the rate limits the fake enforces. A zero field takes the
// documented github.com value; a negative one turns that limit off.
//
// Sources: docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api
// and docs.github.com/en/graphql/overview/rate-limits-and-query-limits-for-the-graphql-api.
type Limits struct {
	// Disabled turns every limit off and drops the x-ratelimit-* headers
	// (GitHub Enterprise Server's default).
	Disabled bool
	// Primary limits, per hour: an installation (5000), a user token
	// (5000), anonymous requests (60), GraphQL points of any identity
	// (5000).
	InstallationPerHour  int
	UserPerHour          int
	AnonymousPerHour     int
	GraphQLPointsPerHour int
	// Secondary limits, per minute: REST points (900; GET, HEAD and
	// OPTIONS cost 1, other methods 5) and GraphQL points (2000; a query
	// costs 1, a mutation 5).
	RESTPointsPerMinute    int
	GraphQLPointsPerMinute int
	// Content creation, per identity: 80 a minute and 500 an hour.
	ContentPerMinute int
	ContentPerHour   int
	// TokenMintsPerHour bounds installation token requests per App (2000
	// OAuth token requests an hour; that it covers installation tokens is
	// assumed).
	TokenMintsPerHour int
}

// withDefaults fills zero fields with the documented values.
func (l Limits) withDefaults() Limits {
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&l.InstallationPerHour, 5000)
	def(&l.UserPerHour, 5000)
	def(&l.AnonymousPerHour, 60)
	def(&l.GraphQLPointsPerHour, 5000)
	def(&l.RESTPointsPerMinute, 900)
	def(&l.GraphQLPointsPerMinute, 2000)
	def(&l.ContentPerMinute, 80)
	def(&l.ContentPerHour, 500)
	def(&l.TokenMintsPerHour, 2000)
	return l
}

// Usage counts what one identity did (see Server.Usage).
type Usage struct {
	// Requests is every REST and GraphQL request admitted; Writes those
	// with a method other than GET, HEAD and OPTIONS, and GraphQL
	// mutations.
	Requests, Writes int
	// ContentCreated counts requests that create content: pull requests,
	// comments, labels added to pull requests, labels, API commits and
	// refs created or updated through the API (which Git Data calls count
	// is not documented: see the package documentation).
	ContentCreated int
	GraphQLPoints  int
	Pushes         int
	TokenMints     int
}

// window is a fixed primary window.
type window struct {
	start time.Time
	used  int
}

// stamp is a use of points at a time.
type stamp struct {
	at     time.Time
	points int
}

// limiter keeps the rate limit state. All methods are called with the
// Server's mu held.
type limiter struct {
	cfg     Limits
	primary map[string]*window
	points  map[string][]stamp
	content map[string][]stamp
	mints   map[int64][]stamp
	usage   map[string]*Usage
}

// newLimiter returns the limiter of opts (nil: the flavor's default).
func newLimiter(opts *Limits, f Flavor) *limiter {
	var cfg Limits
	switch {
	case opts != nil:
		cfg = *opts
	case f == GHES:
		cfg.Disabled = true
	}
	return &limiter{cfg: cfg.withDefaults(), primary: map[string]*window{}, points: map[string][]stamp{},
		content: map[string][]stamp{}, mints: map[int64][]stamp{}, usage: map[string]*Usage{}}
}

// use returns the usage record of an identity key.
func (l *limiter) use(key string) *Usage {
	u := l.usage[key]
	if u == nil {
		u = &Usage{}
		l.usage[key] = u
	}
	return u
}

// Usage returns what the identity did: "anonymous", "app/<slug>",
// "installation/<id>" or "user/<login>" (see Request.Identity).
func (s *Server) Usage(identity string) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.limits.usage[identity]; u != nil {
		return *u
	}
	return Usage{}
}

// restPoints is the secondary-limit cost of a REST method.
func restPoints(method string) int {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return 1
	}
	return 5
}

// primaryLimit is the hourly limit of an identity for a resource.
func (l *limiter) primaryLimit(id *identity, resource string) int {
	if resource == "graphql" {
		return l.cfg.GraphQLPointsPerHour
	}
	switch {
	case id == nil:
		return l.cfg.AnonymousPerHour
	case id.tok != nil && id.tok.kind == tokenInstallation, id.app != nil:
		return l.cfg.InstallationPerHour
	}
	return l.cfg.UserPerHour
}

// primaryMessage is the message of an exhausted primary limit.
func primaryMessage(id *identity) string {
	switch {
	case id == nil:
		return "API rate limit exceeded for 127.0.0.1. (But here's the good news: Authenticated requests get a higher rate limit. Check out the documentation for more details.)"
	case id.tok != nil && id.tok.kind == tokenInstallation:
		return "API rate limit exceeded for installation ID " + strconv.FormatInt(id.tok.inst.id, 10) + "."
	case id.app != nil:
		return "API rate limit exceeded for app ID " + strconv.FormatInt(id.app.id, 10) + "."
	}
	return "API rate limit exceeded for user ID " + strconv.FormatInt(id.who.id, 10) + "."
}

// Secondary limit messages (public reports; the documentation only says
// "an error message that indicates that you exceeded a secondary rate
// limit").
const (
	secondaryMessage = "You have exceeded a secondary rate limit. Please wait a few minutes before you try again. " +
		"If you reach out to GitHub Support for help, please include the request ID."
	contentMessage = "You have exceeded a secondary rate limit and have been temporarily blocked from content creation. " +
		"Please retry your request again later. If you reach out to GitHub Support for help, please include the request ID."
)

// admit counts a request against the limits of id for resource ("core"
// or "graphql"): one unit of the primary limit (a GraphQL query costs one
// point: the fake's queries are small) and points of the secondary one
// (restPoints, or 1 for a GraphQL query and 5 for a mutation). It returns
// the refusal, if any, and the x-ratelimit-* headers. free requests (GET
// /rate_limit) count against nothing.
func (l *limiter) admit(now time.Time, id *identity, points int, free bool, resource string) (*response, http.Header) {
	key := identityKey(id)
	if l.cfg.Disabled {
		if !free {
			l.count(key, points, resource)
		}
		return nil, nil
	}
	w := l.window(now, resource+":"+key)
	limit := l.primaryLimit(id, resource)
	const cost = 1
	header := func() http.Header {
		remaining := max(limit-w.used, 0)
		if limit < 0 {
			return nil
		}
		return http.Header{
			"X-Ratelimit-Limit":     {strconv.Itoa(limit)},
			"X-Ratelimit-Remaining": {strconv.Itoa(remaining)},
			"X-Ratelimit-Used":      {strconv.Itoa(w.used)},
			"X-Ratelimit-Reset":     {strconv.FormatInt(w.start.Add(time.Hour).Unix(), 10)},
			"X-Ratelimit-Resource":  {resource},
		}
	}
	if free {
		return nil, header()
	}
	if limit >= 0 && w.used+cost > limit {
		resp := apiError(http.StatusForbidden, primaryMessage(id))
		return &resp, header()
	}
	perMinute := l.cfg.RESTPointsPerMinute
	if resource == "graphql" {
		perMinute = l.cfg.GraphQLPointsPerMinute
	}
	skey := resource + ":" + key
	if wait, over := overBudget(now, l.points[skey], points, perMinute, time.Minute); over {
		resp := apiError(http.StatusForbidden, secondaryMessage)
		resp.header = http.Header{"Retry-After": {retryAfter(wait)}}
		return &resp, header()
	}
	l.points[skey] = append(prune(now, l.points[skey], time.Minute), stamp{at: now, points: points})
	w.used += cost
	l.count(key, points, resource)
	return nil, header()
}

// count updates the usage of key.
func (l *limiter) count(key string, points int, resource string) {
	u := l.use(key)
	u.Requests++
	if resource == "graphql" {
		u.GraphQLPoints++
	}
	if points >= 5 {
		u.Writes++
	}
}

// window returns the current primary window of key.
func (l *limiter) window(now time.Time, key string) *window {
	w := l.primary[key]
	if w == nil || !now.Before(w.start.Add(time.Hour)) {
		w = &window{start: now}
		l.primary[key] = w
	}
	return w
}

// content admits one content-creating request of id. It returns the
// refusal when a content creation limit is reached.
func (l *limiter) contentCreated(now time.Time, id *identity) *response {
	key := identityKey(id)
	if !l.cfg.Disabled {
		list := prune(now, l.content[key], time.Hour)
		wait, over := overBudget(now, list, 1, l.cfg.ContentPerMinute, time.Minute)
		if w, o := overBudget(now, list, 1, l.cfg.ContentPerHour, time.Hour); o && w > wait {
			wait, over = w, true
		}
		if over {
			resp := apiError(http.StatusForbidden, contentMessage)
			resp.header = http.Header{"Retry-After": {retryAfter(wait)}}
			return &resp
		}
		l.content[key] = append(list, stamp{at: now, points: 1})
	}
	l.use(key).ContentCreated++
	return nil
}

// mint admits one installation token request of an App.
func (l *limiter) mint(now time.Time, a *app, key string) *response {
	if !l.cfg.Disabled {
		list := prune(now, l.mints[a.id], time.Hour)
		if wait, over := overBudget(now, list, 1, l.cfg.TokenMintsPerHour, time.Hour); over {
			resp := apiError(http.StatusForbidden, secondaryMessage)
			resp.header = http.Header{"Retry-After": {retryAfter(wait)}}
			return &resp
		}
		l.mints[a.id] = append(list, stamp{at: now, points: 1})
	}
	l.use(key).TokenMints++
	return nil
}

// overBudget reports whether adding points to the stamps within span
// before now exceeds limit (a negative limit never does), and how long
// until enough of them leave the window.
func overBudget(now time.Time, list []stamp, points, limit int, span time.Duration) (time.Duration, bool) {
	if limit < 0 {
		return 0, false
	}
	total := points
	for _, st := range list {
		if now.Sub(st.at) < span {
			total += st.points
		}
	}
	if total <= limit {
		return 0, false
	}
	excess := total - limit
	for _, st := range list {
		if now.Sub(st.at) >= span {
			continue
		}
		excess -= st.points
		if excess <= 0 {
			return st.at.Add(span).Sub(now), true
		}
	}
	return span, true
}

// prune drops stamps older than span.
func prune(now time.Time, list []stamp, span time.Duration) []stamp {
	i := 0
	for i < len(list) && now.Sub(list[i].at) >= span {
		i++
	}
	return list[i:]
}

// retryAfter formats a wait in whole seconds, at least 1.
func retryAfter(d time.Duration) string {
	sec := int((d + time.Second - 1) / time.Second)
	return strconv.Itoa(max(sec, 1))
}

// identityKey is the Request.Identity of id.
func identityKey(id *identity) string {
	if id == nil {
		return "anonymous"
	}
	return id.key()
}
