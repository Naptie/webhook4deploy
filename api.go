package main

import (
	"encoding/json"
	"net/http"

	"github.com/adnanh/webhook/internal/deploy"
	"github.com/gorilla/mux"
)

// deploymentAPI serves the records of the deployments.
//
// This is the answer to the question a publish workflow keeps asking: did
// this deployment go out, and if not, why not. The record is written while
// the deployment runs, so it can be read back at any moment — including while
// it is still running.
type deploymentAPI struct {
	store *deploy.Store
}

// list serves the summaries of the known deployments, newest first.
func (a *deploymentAPI) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.store.List())
}

// get serves one deployment.
func (a *deploymentAPI) get(w http.ResponseWriter, r *http.Request) {
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
