package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vorash/vora/internal/secret"
)

// errSecretsNotConfigured is returned when the API has no secret store, which
// happens when VORA_MASTER_KEY is unset. Brokering stays disabled rather than
// silently falling back to a weaker store.
var errSecretsNotConfigured = errors.New("credential brokering is not configured (set VORA_MASTER_KEY)")

// secrets returns the configured secret store, if any.
func (s *Server) secrets() secret.Store { return s.opts.Secrets }

// Errors explaining why a secret operation has no tenant to work with. They are
// returned verbatim to the caller: the current "cannot manage secrets for
// another tenant" wording sent operators looking for a tenant conflict when the
// real problem was a global administrator key naming no tenant at all.
var (
	errTenantRequired  = errors.New("a global administrator key must name the tenant: pass --tenant <id>, or use a key scoped to that tenant")
	errTenantForbidden = errors.New("a key scoped to one tenant cannot manage another tenant's secrets")
)

// secretTenant resolves the tenant a secret operation applies to. A global
// administrator must name the tenant; a tenant-scoped administrator is pinned
// to its own tenant and cannot name another.
func secretTenant(caller APIKey, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if isGlobalAdmin(caller) {
		if requested == "" {
			return "", errTenantRequired
		}
		return requested, nil
	}
	if caller.TenantID == "" {
		return "", errTenantForbidden
	}
	if requested != "" && requested != caller.TenantID {
		return "", errTenantForbidden
	}
	return caller.TenantID, nil
}

// createSecret stores a brokered credential. The value is write-only: it is
// sealed immediately and never returned.
func (s *Server) createSecret(w http.ResponseWriter, r *http.Request) {
	store := s.secrets()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, errSecretsNotConfigured.Error())
		return
	}
	var body struct {
		Alias    string `json:"alias"`
		Host     string `json:"host"`
		Scheme   string `json:"scheme"`
		Header   string `json:"header"`
		Value    string `json:"value"`
		TenantID string `json:"tenant_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenant, err := secretTenant(principalFrom(r.Context()).key, body.TenantID)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	rec := secret.Record{
		TenantID: tenant,
		Alias:    strings.TrimSpace(body.Alias),
		Host:     strings.TrimSpace(body.Host),
		Scheme:   strings.TrimSpace(body.Scheme),
		Header:   strings.TrimSpace(body.Header),
	}
	if err := rec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := secret.ValidateValue(body.Value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := store.Put(r.Context(), rec, body.Value); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stored, err := store.Get(r.Context(), tenant, rec.Alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"secret": stored})
}

// listSecrets returns a tenant's credentials, metadata only.
func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	store := s.secrets()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, errSecretsNotConfigured.Error())
		return
	}
	tenant, err := secretTenant(principalFrom(r.Context()).key, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	records, err := store.List(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if records == nil {
		records = []secret.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": records})
}

// deleteSecret removes a tenant's credential.
func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	store := s.secrets()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, errSecretsNotConfigured.Error())
		return
	}
	tenant, err := secretTenant(principalFrom(r.Context()).key, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	alias := r.PathValue("alias")
	if err := store.Delete(r.Context(), tenant, alias); err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			writeError(w, http.StatusNotFound, "secret not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
