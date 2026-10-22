package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/adnanh/webhook/internal/deploy"
	"github.com/adnanh/webhook/internal/hook"
	"github.com/gorilla/mux"
)

func TestDeploymentAPIList(t *testing.T) {
	store, err := deploy.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&deploy.Deployment{
		ID:        "12345678901",
		App:       "example",
		RunID:     "12345678901",
		Status:    deploy.StatusSucceeded,
		StartedAt: time.Now().UTC(),
	})

	server := httptest.NewServer(routesFor(store))
	defer server.Close()

	body, code := get(t, server.URL+"/deployments")
	if code != http.StatusOK {
		t.Fatalf("the list answered with %d", code)
	}

	var list []deploy.Summary
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("the list is not a list of deployments: %v", err)
	}

	if len(list) != 1 {
		t.Fatalf("the list holds %d deployments, want 1", len(list))
	}

	if list[0].ID != "12345678901" || list[0].Status != deploy.StatusSucceeded {
		t.Errorf("the list holds %+v", list[0])
	}
}

func TestDeploymentAPIGet(t *testing.T) {
	store, err := deploy.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&deploy.Deployment{
		ID:        "1",
		App:       "example",
		Status:    deploy.StatusFailed,
		Reason:    "the new build did not answer",
		StartedAt: time.Now().UTC(),
	})

	server := httptest.NewServer(routesFor(store))
	defer server.Close()

	body, code := get(t, server.URL+"/deployments/1")
	if code != http.StatusOK {
		t.Fatalf("the deployment answered with %d", code)
	}

	var record deploy.Deployment
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatalf("the deployment is not a deployment: %v", err)
	}

	if record.Reason != "the new build did not answer" {
		t.Errorf("the reason is %q", record.Reason)
	}
}

func TestDeploymentAPIGetUnknown(t *testing.T) {
	store, err := deploy.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	server := httptest.NewServer(routesFor(store))
	defer server.Close()

	if _, code := get(t, server.URL+"/deployments/nothing"); code != http.StatusNotFound {
		t.Errorf("a deployment that does not exist answered with %d, want %d", code, http.StatusNotFound)
	}
}

// TestExampleConfig loads the example configuration of a host. It is the
// only test that knows about the configuration as it is written down, so a
// mistake in it is caught here rather than on the host.
func TestExampleConfig(t *testing.T) {
	hooks := hook.Hooks{}

	if err := hooks.LoadFromFile("examples/deployments.yaml", false); err != nil {
		t.Fatalf("the example configuration could not be loaded: %v", err)
	}

	if len(hooks) == 0 {
		t.Fatal("the example configuration holds no hooks")
	}

	for _, h := range hooks {
		if h.Deployment == nil {
			t.Errorf("the hook %q has no deployment", h.ID)
			continue
		}

		if err := h.Deployment.Validate(); err != nil {
			t.Errorf("the deployment of %q is not usable: %v", h.ID, err)
		}
	}
}

// routesFor returns the routes of the deployment API.
func routesFor(store *deploy.Store) *mux.Router {
	api := &deploymentAPI{store: store}

	router := mux.NewRouter()
	router.HandleFunc("/deployments", api.list)
	router.HandleFunc("/deployments/{id}", api.get)
	router.HandleFunc("/deployments/{id}/log", api.log)

	return router
}

// get asks a server for a page.
func get(t *testing.T, url string) ([]byte, int) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	defer resp.Body.Close()

	body := make([]byte, 0)
	buf := make([]byte, 4096)

	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}

	return body, resp.StatusCode
}
