package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/adnanh/webhook/internal/deploy"
	"github.com/gorilla/mux"
)

// deploymentAPI serves the records of the deployments.
//
// This is the answer to the question a publish workflow keeps asking: did
// this deployment go out, and if not, why not. The record is written while
// the deployment runs, so it can be read back at any moment — including while
// it is still running.
//
// The service is reachable from the outside, and what it serves says which
// applications the host runs, which ports they are on and how they were
// deployed. It is therefore closed unless the caller presents the token: a
// request without it is refused, and so is every request when no token is
// configured at all.
type deploymentAPI struct {
	store *deploy.Store
	// token is the secret the caller has to present.
	token string
}

// authorized reports whether the request carries the token.
//
// The token is accepted either as a bearer token or in the header the hooks
// use, so that a caller who already holds the shared secret does not have to
// learn a second one.
func (a *deploymentAPI) authorized(r *http.Request) bool {
	if a.token == "" {
		return false
	}

	presented := r.Header.Get("X-Deployment-Token")
	if presented == "" {
		presented = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}

	return subtle.ConstantTimeCompare([]byte(presented), []byte(a.token)) == 1
}

// refuse turns a request away.
func (a *deploymentAPI) refuse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="deployments"`)
	http.Error(w, "the deployment API needs a token", http.StatusUnauthorized)
}

// list serves the summaries of the known deployments, newest first.
func (a *deploymentAPI) list(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		a.refuse(w)
		return
	}

	writeJSON(w, http.StatusOK, a.store.List())
}

// get serves one deployment.
func (a *deploymentAPI) get(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		a.refuse(w)
		return
	}

	id := mux.Vars(r)["id"]

	record, ok := a.store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "there is no deployment with that id"})
		return
	}

	writeJSON(w, http.StatusOK, record)
}

// log serves the log of one deployment.
func (a *deploymentAPI) log(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		a.refuse(w)
		return
	}

	id := mux.Vars(r)["id"]

	record, ok := a.store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "there is no deployment with that id"})
		return
	}

	body, err := a.store.Log(record.App, record.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "the log of that deployment is not available"})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// writeJSON writes a value as JSON.
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		http.Error(w, "the answer could not be written", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(body)
}
