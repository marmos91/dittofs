package metrics

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ServerOptions configures the dedicated metrics listener. It is decoupled from
// config.MetricsConfig so this package carries no config dependency; the caller
// translates config into these plain fields.
type ServerOptions struct {
	Addr  string // host:port to bind
	Path  string // HTTP path (e.g. /metrics)
	Token string // bearer token; empty = unauthenticated

	// TLS (optional). Both CertFile and KeyFile must be set to enable HTTPS.
	CertFile   string
	KeyFile    string
	ClientCA   string // PEM bundle; when set, requires+verifies client certs (mTLS)
	MinVersion string // "1.2" | "1.3"; empty = 1.2
}

// Server runs the metrics endpoint on its own HTTP listener. Its lifecycle
// mirrors the control-plane API server: Serve blocks until the context is
// cancelled, then drains gracefully.
type Server struct {
	http *http.Server
	tls  *tls.Config
}

// NewServer builds a metrics Server serving m's registry under opts.Path, with
// optional bearer-token auth and TLS.
func NewServer(m *Metrics, opts ServerOptions) (*Server, error) {
	mux := http.NewServeMux()
	mux.Handle(opts.Path, withAuth(opts.Token, m.Handler()))

	tlsCfg, err := buildTLSConfig(opts)
	if err != nil {
		return nil, err
	}

	return &Server{
		http: &http.Server{
			Addr:              opts.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			TLSConfig:         tlsCfg,
		},
		tls: tlsCfg,
	}, nil
}

// Serve starts the listener and blocks until ctx is cancelled, then shuts down
// gracefully. Returns nil on graceful shutdown.
func (s *Server) Serve(ctx context.Context) error {
	errChan := make(chan error, 1)
	go func() {
		scheme := "http"
		if s.tls != nil {
			scheme = "https"
		}
		logger.Info("metrics server listening", "addr", s.http.Addr, "scheme", scheme)
		var err error
		if s.tls != nil {
			err = s.http.ListenAndServeTLS("", "")
		} else {
			err = s.http.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			select {
			case errChan <- err:
			default:
			}
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics server shutdown error", "error", err)
			return err
		}
		logger.Info("metrics server stopped gracefully")
		return nil
	case err := <-errChan:
		return fmt.Errorf("metrics server failed: %w", err)
	}
}

// withAuth wraps h with bearer-token authentication when token is non-empty.
func withAuth(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// buildTLSConfig returns a *tls.Config when cert+key are set, else nil (plain
// HTTP). When ClientCA is set it requires and verifies a client certificate.
func buildTLSConfig(opts ServerOptions) (*tls.Config, error) {
	if opts.CertFile == "" && opts.KeyFile == "" {
		return nil, nil
	}
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, fmt.Errorf("metrics TLS requires both cert and key files")
	}
	cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load metrics TLS keypair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tlsVersion(opts.MinVersion),
	}
	if opts.ClientCA != "" {
		pem, err := os.ReadFile(opts.ClientCA)
		if err != nil {
			return nil, fmt.Errorf("read metrics client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("metrics client CA %q contains no usable certificates", opts.ClientCA)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

func tlsVersion(v string) uint16 {
	if strings.TrimSpace(v) == "1.3" {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}
