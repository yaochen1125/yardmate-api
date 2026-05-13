package main

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// Server bundles the chi router with the verifier and vault it serves. Tests
// construct it via newServer with a test root pool + synthetic vault, the
// main entry point constructs it from real production values.
type Server struct {
	verifier *attest.Verifier
	vault    *secrets.Vault
	router   chi.Router
}

func newServer(verifier *attest.Verifier, vault *secrets.Vault) *Server {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Second))

	r.Get("/healthz", healthz)
	r.Post("/v1/attest/challenge", handleAttestChallenge(verifier))
	r.Post("/v1/attest/register", handleAttestRegister(verifier))
	r.Post("/v1/secrets/challenge", handleSecretsChallenge(verifier))
	r.Post("/v1/app-secrets", handleAppSecrets(verifier, vault))

	return &Server{verifier: verifier, vault: vault, router: r}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
