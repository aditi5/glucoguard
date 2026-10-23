package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Principal is the authenticated caller.
type Principal struct {
	ID   string
	Role string // "patient" or "clinician"
}

type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

var errUnauthenticated = errors.New("unauthenticated")

// DevHeaderAuth trusts "Authorization: Dev <role>:<id>". It exists only for
// local development and tests; main.go refuses to use it in production.
// Replace with phone-OTP / OIDC token verification before any real use.
type DevHeaderAuth struct{}

func (DevHeaderAuth) Authenticate(r *http.Request) (Principal, error) {
	h := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(h, "Dev ")
	if !ok {
		return Principal{}, errUnauthenticated
	}
	role, id, ok := strings.Cut(rest, ":")
	if !ok || id == "" || (role != "patient" && role != "clinician") {
		return Principal{}, errUnauthenticated
	}
	return Principal{ID: id, Role: role}, nil
}

type ctxKey int

const (
	principalKey ctxKey = iota
	requestIDKey
)

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

func (s *Server) requireAuth(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Auth.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid credentials")
			return
		}
		if role != "" && p.Role != role {
			writeError(w, http.StatusForbidden, "forbidden", "this endpoint requires role "+role)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

// common wraps every request: request ID, security headers, body limit,
// panic recovery, rate limiting and access logging. Logs never include
// request bodies, because they carry health data.
func (s *Server) common(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var b [8]byte
		_, _ = rand.Read(b[:])
		rid := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-ID", rid)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic", "request_id", rid, "panic", v)
				writeError(rec, http.StatusInternalServerError, "internal", "internal error")
			}
			s.Log.Info("http", "request_id", rid, "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		if !s.limiter.allow(clientKey(r)) {
			writeError(rec, http.StatusTooManyRequests, "rate_limited", "too many requests; please retry shortly")
			return
		}
		r.Body = http.MaxBytesReader(rec, r.Body, 64<<10)
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestIDKey, rid)))
	})
}

func clientKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return h
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a per-key token bucket. Safety-relevant writes are cheap, so
// the limit is generous; it exists to stop abuse, not normal use.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{rate: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 100000 { // crude memory bound
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
