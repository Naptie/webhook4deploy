package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeRoute is one route of the fake proxy.
type fakeRoute struct {
	// host is the host the route matches. An empty host means the route
	// matches every host.
	host string
	// dial is the address the route proxies to.
	dial string
}

// fakeCaddy is a stand-in for the admin API of Caddy. It holds the dial
// address of every upstream and serves the configuration in the same shape
// the real one does.
type fakeCaddy struct {
	routes []fakeRoute
	server *httptest.Server
}

// newFakeCaddy starts a fake admin API with the given routes.
func newFakeCaddy(t *testing.T, routes []fakeRoute) *fakeCaddy {
	t.Helper()

	c := &fakeCaddy{routes: routes}

	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/config"), "/")

		switch {
		case r.Method == http.MethodGet && path == "":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(c.config())

		case r.Method == http.MethodGet:
			value, ok := c.dial(path)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(value)

		case r.Method == http.MethodPatch:
			var value string
			if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !c.set(path, value) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	t.Cleanup(c.server.Close)

	return c
}

// path returns the configuration path of a route.
func (c *fakeCaddy) path(index int) string {
	return "/apps/http/servers/srv1/routes/" + strconv.Itoa(index) + "/handle/0/upstreams/0/dial"
}

// dial returns the address a route proxies to.
func (c *fakeCaddy) dial(path string) (string, bool) {
	for i := range c.routes {
		if c.path(i) == path {
			return c.routes[i].dial, true
		}
	}

	return "", false
}

// set changes the address a route proxies to.
func (c *fakeCaddy) set(path string, value string) bool {
	for i := range c.routes {
		if c.path(i) == path {
			c.routes[i].dial = value
			return true
		}
	}

	return false
}

// config builds the configuration the fake serves.
func (c *fakeCaddy) config() map[string]any {
	routes := make([]any, 0, len(c.routes))

	for _, route := range c.routes {
		entry := map[string]any{}

		if route.host != "" {
			entry["match"] = []any{map[string]any{"host": []any{route.host}}}
		}

		entry["handle"] = []any{
			map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": route.dial}},
			},
		}

		routes = append(routes, entry)
	}

	return map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"srv1": map[string]any{
						"listen": []any{":80"},
						"routes": routes,
					},
				},
			},
		},
	}
}

// client returns a client for the fake.
func (c *fakeCaddy) client() *CaddyClient {
	return &CaddyClient{AdminURL: c.server.URL, HTTP: c.server.Client()}
}

func TestUpstreamsForHosts(t *testing.T) {
	c := newFakeCaddy(t, []fakeRoute{
		{host: "app.example", dial: "localhost:3001"},
		{host: "other.example", dial: "localhost:9999"},
	})

	upstreams, err := c.client().UpstreamsForHosts(context.Background(), []string{"app.example"})
	if err != nil {
		t.Fatalf("UpstreamsForHosts returned %v", err)
	}

	if len(upstreams) != 1 {
		t.Fatalf("UpstreamsForHosts returned %d upstreams, want 1: %v", len(upstreams), upstreams)
	}

	want := "/apps/http/servers/srv1/routes/0/handle/0/upstreams/0/dial"
	if upstreams[0].Path != want {
		t.Errorf("the upstream is at %q, want %q", upstreams[0].Path, want)
	}

	if upstreams[0].Dial != "localhost:3001" {
		t.Errorf("the upstream dials %q, want %q", upstreams[0].Dial, "localhost:3001")
	}
}

func TestUpstreamsForSeveralHosts(t *testing.T) {
	c := newFakeCaddy(t, []fakeRoute{
		{host: "app.example", dial: "localhost:3001"},
		{host: "app.example", dial: "localhost:3001"},
		{host: "other.example", dial: "localhost:9999"},
	})

	upstreams, err := c.client().UpstreamsForHosts(context.Background(), []string{"app.example", "app.example"})
	if err != nil {
		t.Fatalf("UpstreamsForHosts returned %v", err)
	}

	if len(upstreams) != 2 {
		t.Fatalf("UpstreamsForHosts returned %d upstreams, want 2: %v", len(upstreams), upstreams)
	}
}

func TestUpstreamsWithDial(t *testing.T) {
	c := newFakeCaddy(t, []fakeRoute{
		{host: "app.example", dial: "localhost:3001"},
		// A route that does not name the application but proxies to it, the
		// way a loopback alias does.
		{dial: "localhost:3001"},
		{host: "other.example", dial: "localhost:9999"},
	})

	paths, err := c.client().UpstreamsWithDial(context.Background(), "localhost:3001")
	if err != nil {
		t.Fatalf("UpstreamsWithDial returned %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("UpstreamsWithDial returned %d paths, want 2: %v", len(paths), paths)
	}
}

func TestSwitch(t *testing.T) {
	c := newFakeCaddy(t, []fakeRoute{
		{host: "app.example", dial: "localhost:3001"},
		{dial: "localhost:3001"},
	})

	client := c.client()
	ctx := context.Background()

	paths, err := client.UpstreamsWithDial(ctx, "localhost:3001")
	if err != nil {
		t.Fatalf("UpstreamsWithDial returned %v", err)
	}

	if err := client.Switch(ctx, paths, "localhost:3000"); err != nil {
		t.Fatalf("Switch returned %v", err)
	}

	for i, route := range c.routes {
		if route.dial != "localhost:3000" {
			t.Errorf("the upstream %d is %q after the switch, want %q", i, route.dial, "localhost:3000")
		}
	}
}

func TestSwitchUnknownPath(t *testing.T) {
	c := newFakeCaddy(t, []fakeRoute{{host: "app.example", dial: "localhost:3001"}})

	err := c.client().Switch(context.Background(), []string{"/apps/http/servers/srv9/routes/0/dial"}, "localhost:3000")
	if err == nil {
		t.Fatal("Switch did not report that the upstream could not be set")
	}
}

func TestFreePort(t *testing.T) {
	port, err := FreePort([]int{7000, 7100}, 0)
	if err != nil {
		t.Fatalf("FreePort returned %v", err)
	}

	if port < 7000 || port >= 7100 {
		t.Errorf("FreePort returned %d, which is outside the range", port)
	}
}

func TestFreePortSkipsThePortInUse(t *testing.T) {
	port, err := FreePort([]int{7000, 7101}, 7000)
	if err != nil {
		t.Fatalf("FreePort returned %v", err)
	}

	if port == 7000 {
		t.Error("FreePort returned the port that is in use")
	}
}

func TestPortOf(t *testing.T) {
	port, err := portOf("localhost:3001")
	if err != nil {
		t.Fatalf("portOf returned %v", err)
	}

	if port != 3001 {
		t.Errorf("portOf returned %d, want %d", port, 3001)
	}

	if _, err := portOf("localhost"); err == nil {
		t.Error("portOf did not report that the address has no port")
	}
}
