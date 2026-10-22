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

// theToken is the secret the deployment API is closed behind.
const theToken = "a-shared-secret"

func TestDeploymentAPIList(t *testing.T) {
	store, err := deploy.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&deploy.Deployment{
		ID:        "39607416602",
		App:       "nearcade",
		RunID:     "39607416602",
		Status:    deploy.StatusSucceeded,
		StartedAt: time.Now().UTC(),
	})

	server := httptest.NewServer(routesFor(store, theToken))
	defer server.Close()

	body, code := get(t, server.URL+"/deployments", theToken)
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

	if list[0].ID != "39607416602" || list[0].Status != deploy.StatusSucceeded {
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
		App:       "nearcade",
		Status:    deploy.StatusFailed,
		Reason:    "the new build did not answer",
		StartedAt: time.Now().UTC(),
	})

	server := httptest.NewServer(routesFor(store, theToken))
	defer server.Close()

	body, code := get(t, server.URL+"/deployments/1", theToken)
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

	server := httptest.NewServer(routesFor(store, theToken))
	defer server.Close()

	if _, code := get(t, server.URL+"/deployments/nothing", theToken); code != http.StatusNotFound {
		t.Errorf("a deployment that does not exist answered with %d, want %d", code, http.StatusNotFound)
	}
}

// TestDeploymentAPIIsClosed is the reason the API needs a token at all: the
// service is reachable from the outside, and what it serves says which
// applications the host runs.
func TestDeploymentAPIIsClosed(t *testing.T) {
	store, err := deploy.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	server := httptest.NewServer(routesFor(store, theToken))
	defer server.Close()

	cases := []struct {
		name  string
		token string
	}{
		{name: "without a token", token: ""},
		{name: "with the wrong token", token: "not-the-token"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, path := range []string{"/deployments", "/deployments/1", "/deployments/1/log"} {
				if _, code := get(t, server.URL+path, c.token); code != http.StatusUnauthorized {
					t.Errorf("%s answered with %d, want %d", path, code, http.StatusUnauthorized)
				}
			}
		})
	}
}

// TestDeploymentAPIIsClosedWithoutAToken: when no token is configured, the
// API is closed even for a caller who presents one. A service that forgot to
// be configured must not serve.
func TestDeploymentAPIIsClosedWithoutAToken(t *testing.T) {
	store, err := deploy.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	server := httptest.NewServer(routesFor(store, ""))
	defer server.Close()

	if _, code := get(t, server.URL+"/deployments", theToken); code != http.StatusUnauthorized {
		t.Errorf("the list answered with %d, want %d", code, http.StatusUnauthorized)
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
func routesFor(store *deploy.Store, token string) *mux.Router {
	api := &deploymentAPI{store: store, token: token}

	router := mux.NewRouter()
	router.HandleFunc("/deployments", api.list)
	router.HandleFunc("/deployments/{id}", api.get)
	router.HandleFunc("/deployments/{id}/log", api.log)

	return router
}

// get asks a server for a page, presenting the token.
func get(t *testing.T, url string, token string) ([]byte, int) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("the request could not be made: %v", err)
	}

	if token != "" {
		request.Header.Set("X-Deployment-Token", token)
	}

	resp, err := http.DefaultClient.Do(request)
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
