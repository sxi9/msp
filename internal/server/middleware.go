package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/store"
)

type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeyIP
)

// Role names, ordered by roleRank.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// cspValue is the Content-Security-Policy sent on every response.
const cspValue = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// clientIP returns the caller's address: the value the request-log middleware
// resolved (first X-Forwarded-For when cfg.TrustProxy, else the RemoteAddr
// host) or, outside the middleware chain, the RemoteAddr host.
func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(ctxKeyIP).(string); ok && ip != "" {
		return ip
	}
	return remoteHost(r)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// resolveIP applies the TrustProxy rule once per request.
func (s *Server) resolveIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if first != "" {
				return first
			}
		}
	}
	return remoteHost(r)
}

// statusWriter records the status code for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// recoverPanic turns a handler panic into a 500 with a logged stack.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLog resolves the client IP into the context and logs one line per
// request. Successful agent syncs (one per device per interval, by design the
// bulk of all traffic) are logged at Debug; everything else at Info.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ip := s.resolveIP(r)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyIP, ip))
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		ms := time.Since(start).Milliseconds()
		if status == http.StatusOK && r.URL.Path == "/api/agent/v1/sync" {
			s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", status, "ms", ms, "ip", ip)
			return
		}
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", status, "ms", ms, "ip", ip)
	})
}

// securityHeaders sets the CSP and the other protective headers on every
// response; HSTS only when the server itself terminates TLS.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	hsts := s.cfg.TLSCert != "" && s.cfg.TLSKey != ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", cspValue)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// bodyLimit caps request bodies: 256 KiB under /api/agent/, 512 KiB elsewhere.
func (s *Server) bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			// The file-library upload (SD1) streams an artifact far larger than a
			// JSON request; it enforces its own MSP_MAX_FILE_MB cap in the handler
			// (dashboard_files.go), so it is exempt from the generic limit here.
			if r.Method == http.MethodPost && r.URL.Path == "/api/v1/files" {
				next.ServeHTTP(w, r)
				return
			}
			limit := int64(512 << 10)
			if strings.HasPrefix(r.URL.Path, "/api/agent/") {
				limit = 256 << 10
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// sessionAuth resolves the msp_session cookie to a user and stores it in the
// context; 401 unauthorized otherwise.
func (s *Server) sessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		u, err := s.st.GetSession(r.Context(), c.Value)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("session lookup", "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser, &u)))
	})
}

// userFrom returns the session user placed by sessionAuth, or nil.
func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxKeyUser).(*store.User)
	return u
}

// roleRank orders roles: viewer 0, operator 1, admin 2; unknown -1.
func roleRank(role string) int {
	switch role {
	case RoleViewer:
		return 0
	case RoleOperator:
		return 1
	case RoleAdmin:
		return 2
	}
	return -1
}

// requireRole answers 403 forbidden unless the session user's role ranks at
// least min. It must run inside sessionAuth.
func (s *Server) requireRole(min string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r.Context())
		if u == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		if roleRank(u.Role) < roleRank(min) {
			writeError(w, http.StatusForbidden, "forbidden", "")
			return
		}
		h(w, r)
	}
}

// csrf requires the X-Requested-With: msp header on every state-changing
// request. Browsers never add it cross-site without a CORS preflight, which
// the server does not answer, so a forged form post cannot carry it.
func (s *Server) csrf(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("X-Requested-With") != "msp" {
				writeError(w, http.StatusForbidden, "csrf", "")
				return
			}
		}
		h(w, r)
	}
}

// deviceAuth authenticates an agent by its bearer device token and passes the
// device row to the handler; 401 invalid_device_token otherwise (including
// revoked devices, whose token_hash is NULL).
func (s *Server) deviceAuth(h func(w http.ResponseWriter, r *http.Request, dev store.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			writeError(w, http.StatusUnauthorized, "invalid_device_token", "")
			return
		}
		token := strings.TrimSpace(auth[len(prefix):])
		if token == "" {
			writeError(w, http.StatusUnauthorized, "invalid_device_token", "")
			return
		}
		dev, err := s.st.GetDeviceByToken(r.Context(), token)
		if err != nil {
			if !errors.Is(err, store.ErrInvalidToken) {
				s.log.Error("device lookup", "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid_device_token", "")
			return
		}
		h(w, r, dev)
	}
}
