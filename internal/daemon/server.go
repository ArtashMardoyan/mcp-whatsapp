package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pairDriver is the minimal surface the Server needs from the underlying
// WhatsApp client. Production wiring (cmd/whatsapp-mcp/production_driver.go)
// satisfies this via *client.Client. Tests substitute a fake.
type pairDriver interface {
	IsLoggedIn() bool
	// StartPairing begins an unpaired session. onQR is invoked every time
	// whatsmeow emits a new pairing code. onSuccess is invoked once
	// pairing succeeds.
	StartPairing(ctx context.Context, onQR func(code string), onSuccess func()) error
	// Connect attaches event handlers (including LoggedOut) on an already-
	// paired session. onLoggedOut fires when the remote device revokes the
	// session.
	Connect(ctx context.Context, onLoggedOut func()) error
	Logout(ctx context.Context) error
	Disconnect()
}

// Config configures a daemon Server. Addr may be "host:port" or "host:0"
// (the latter only for tests). MCPMount, if non-nil, is called once the
// mux is being built so the caller can attach additional handlers (in
// practice, the /mcp Streamable HTTP handler from internal/mcp).
//
// AuthToken is required and gates /mcp and /pair/* behind the HTTP header
// `Authorization: Bearer <token>` (a browser opens /pair?token=<token> once
// and gets an HttpOnly cookie instead). LoopbackOnly additionally rejects
// any request whose Host is not 127.0.0.1, localhost or [::1] on the bound
// port, which blocks DNS-rebinding attacks from a browser.
type Config struct {
	Addr         string
	Driver       pairDriver
	MCPMount     func(mux *http.ServeMux)
	AuthToken    string
	LoopbackOnly bool
}

// Server is the long-lived daemon process. Safe for a single Run call.
type Server struct {
	cfg   Config
	cache *PairCache

	mu         sync.Mutex
	httpServer *http.Server
	boundAddr  string
	listenerOK chan struct{} // closed once the listener is bound (tests use this)
}

// New constructs a Server but does not start any goroutines yet.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("daemon.New: Addr required")
	}
	if cfg.Driver == nil {
		return nil, errors.New("daemon.New: Driver required")
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("daemon.New: AuthToken required")
	}
	return &Server{
		cfg:        cfg,
		cache:      NewPairCache(),
		listenerOK: make(chan struct{}),
	}, nil
}

// Cache exposes the pair cache so the MCP server can answer pairing_status
// (and for tests). The daemon owns the cache, so callers must construct the
// daemon before wiring the cache into the MCP server.
func (s *Server) Cache() *PairCache { return s.cache }

// SetMCPMount installs the MCP HTTP mount after construction. The daemon owns
// the pair cache, so serve.go builds the daemon first, hands its cache to the
// MCP server, then mounts the MCP handler here. cfg.MCPMount is read in Run,
// so setting it any time before Run takes effect.
func (s *Server) SetMCPMount(fn func(mux *http.ServeMux)) { s.cfg.MCPMount = fn }

// BoundAddr returns the address the HTTP listener actually bound to, useful
// when Addr was "host:0". Only valid after <-s.listenerOK fires.
func (s *Server) BoundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundAddr
}

// Run starts the HTTP listener, begins the pairing state machine, and
// blocks until ctx is cancelled. On cancel, performs the shutdown
// sequence: drain HTTP → Disconnect driver.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	var coder pairCoder
	if pc, ok := s.cfg.Driver.(pairCoder); ok {
		coder = pc
	}
	handlers := newPairHandlers(s.cache, s.cfg.Driver, coder)
	handlers.mount(mux)
	if s.cfg.MCPMount != nil {
		s.cfg.MCPMount(mux)
	} else {
		// No MCP attached (tests). Provide a 503 so the shape is observable.
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "mcp not mounted", http.StatusServiceUnavailable)
		})
	}

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	var allowedHosts []string
	if s.cfg.LoopbackOnly {
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		allowedHosts = []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}
	}
	rootHandler := originGuard(authMiddleware(mux, s.cfg.AuthToken), allowedHosts)

	s.mu.Lock()
	s.httpServer = &http.Server{
		Handler:           rootHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.boundAddr = ln.Addr().String()
	httpSrv := s.httpServer
	s.mu.Unlock()

	listenErrCh := make(chan error, 1)
	go func() { listenErrCh <- httpSrv.Serve(ln) }()

	close(s.listenerOK)

	// Drive the pairing state machine.
	if err := s.bootDriver(ctx); err != nil {
		_ = httpSrv.Shutdown(context.Background())
		return fmt.Errorf("boot driver: %w", err)
	}

	// Wait for cancellation or a listener failure.
	select {
	case <-ctx.Done():
	case err := <-listenErrCh:
		if err != nil && err != http.ErrServerClosed {
			s.cfg.Driver.Disconnect()
			return fmt.Errorf("http listen: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	s.cfg.Driver.Disconnect()
	return nil
}

// bootDriver inspects the driver's IsLoggedIn and either starts pairing
// or connects with the existing session. Safe to call only once per Run.
func (s *Server) bootDriver(ctx context.Context) error {
	onQR := func(code string) { s.cache.SetQR(code) }
	onPairSuccess := func() {
		s.cache.SetPaired()
		// Transition into Connect to install the LoggedOut handler.
		_ = s.cfg.Driver.Connect(ctx, s.onLoggedOut(ctx))
	}

	if s.cfg.Driver.IsLoggedIn() {
		s.cache.SetPaired()
		return s.cfg.Driver.Connect(ctx, s.onLoggedOut(ctx))
	}
	return s.cfg.Driver.StartPairing(ctx, onQR, onPairSuccess)
}

// onLoggedOut returns a callback the driver invokes when the remote device
// revokes the session. Flips the cache to unpaired and kicks the pairing
// flow back up.
func (s *Server) onLoggedOut(ctx context.Context) func() {
	return func() {
		s.cache.Reset()
		onQR := func(code string) { s.cache.SetQR(code) }
		onPairSuccess := func() {
			s.cache.SetPaired()
			_ = s.cfg.Driver.Connect(ctx, s.onLoggedOut(ctx))
		}
		_ = s.cfg.Driver.StartPairing(ctx, onQR, onPairSuccess)
	}
}

// authCookie carries the bearer token for the browser pairing page, which
// cannot set an Authorization header.
const authCookie = "whatsapp_mcp_token"

func authMiddleware(next http.Handler, token string) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// One-time browser login: /pair?token=... sets the cookie, then
		// redirects so the token leaves the address bar and history.
		if q := r.URL.Query().Get("token"); q != "" && r.URL.Path == "/pair" {
			if subtle.ConstantTimeCompare([]byte(q), []byte(token)) != 1 {
				unauthorized(w)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: authCookie, Value: token, Path: "/pair", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/pair", http.StatusSeeOther)
			return
		}
		got := r.Header.Get("Authorization")
		if c, err := r.Cookie(authCookie); err == nil && got == "" {
			got = "Bearer " + c.Value
		}
		if subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-mcp"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// originGuard rejects requests whose Host is not in allowedHosts (skipped
// when allowedHosts is nil, i.e. -allow-remote) and any browser request whose
// Origin is not the same host as the request.
func originGuard(next http.Handler, allowedHosts []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowedHosts != nil && !slices.Contains(allowedHosts, r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func requiresAuth(path string) bool {
	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		return true
	}
	if path == "/pair" || strings.HasPrefix(path, "/pair/") {
		return true
	}
	return false
}
