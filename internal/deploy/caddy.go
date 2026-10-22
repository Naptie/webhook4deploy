package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CaddyClient talks to the admin API of Caddy.
//
// Caddy is the router in front of the applications. Its configuration is
// available as JSON through the admin API, and the upstream a route proxies
// to can be changed at runtime. That is what makes a blue/green deployment
// possible: the new build is started on a free port, and only when it is
// healthy are the routes moved over to it.
type CaddyClient struct {
	// AdminURL is the base URL of the admin API, for example
	// "http://localhost:2019".
	AdminURL string
	// HTTP is the client used for the requests.
	HTTP *http.Client
}

// NewCaddyClient returns a client for the admin API at adminURL.
func NewCaddyClient(adminURL string) *CaddyClient {
	return &CaddyClient{
		AdminURL: strings.TrimSuffix(adminURL, "/"),
		HTTP:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Upstream is one of the routes of Caddy that proxies to an application.
type Upstream struct {
	// Path is the address of the upstream in the admin API, for example
	// "/apps/http/servers/srv1/routes/0/handle/0/upstreams/0/dial".
	Path string
	// Dial is the address the route proxies to, for example
	// "localhost:3001".
	Dial string
}

// config is the shape of the Caddy configuration that matters here. The
// upstream of a route is a list of addresses, and the path of that list is
// what the admin API addresses it by.
//
// It is an alias rather than a defined type, so that the configuration can be
// walked with the same type switches as the rest of the tree.
type config = map[string]any

// UpstreamsForHosts returns the upstreams of the routes that serve the given
// hosts.
//
// The hosts are what an application is reached under. Their routes are found
// by the host matcher of the route, not by the listener: several sites share
// a listener and are told apart by their host.
func (c *CaddyClient) UpstreamsForHosts(ctx context.Context, hosts []string) ([]Upstream, error) {
	cfg, err := c.config(ctx)
	if err != nil {
		return nil, err
	}

	var upstreams []Upstream
	collectHostUpstreams(cfg, "", hosts, &upstreams)

	sort.Slice(upstreams, func(i, j int) bool {
		return upstreams[i].Path < upstreams[j].Path
	})

	return upstreams, nil
}

// UpstreamsWithDial returns the paths of the upstreams that dial target.
//
// The routes of an application are not always found by their host: a
// loopback alias proxies to the application without naming it. Every upstream
// that dials the same address belongs to the same application, so they are
// moved together.
func (c *CaddyClient) UpstreamsWithDial(ctx context.Context, target string) ([]string, error) {
	cfg, err := c.config(ctx)
	if err != nil {
		return nil, err
	}

	var upstreams []Upstream
	collectUpstreams(cfg, "", target, &upstreams)

	paths := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		paths = append(paths, upstream.Path)
	}
	sort.Strings(paths)

	return paths, nil
}

// Switch points every one of the given upstream paths at target and reads
// each one back to confirm the change took.
//
// The read-back matters: the admin API answers a PATCH with the status of the
// request, and a status is not a promise that the configuration now points
// where it should. Reading the value back is what turns the switch into
// something that can be verified.
func (c *CaddyClient) Switch(ctx context.Context, paths []string, target string) error {
	for _, path := range paths {
		if err := c.setUpstream(ctx, path, target); err != nil {
			return err
		}
	}

	for _, path := range paths {
		value, err := c.getUpstream(ctx, path)
		if err != nil {
			return err
		}
		if value != target {
			return fmt.Errorf("the upstream %s is %q after the switch, expected %q", path, value, target)
		}
	}

	return nil
}

// setUpstream writes the dial address of a single upstream.
func (c *CaddyClient) setUpstream(ctx context.Context, path string, target string) error {
	body, err := json.Marshal(target)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.AdminURL+"/config"+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("could not set %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not set %s: Caddy answered with %s", path, resp.Status)
	}

	return nil
}

// getUpstream reads the dial address of a single upstream.
func (c *CaddyClient) getUpstream(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/config"+path, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not read %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not read %s: Caddy answered with %s", path, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var value string
	if err := json.Unmarshal(body, &value); err != nil {
		return "", fmt.Errorf("could not read %s: %w", path, err)
	}

	return value, nil
}

// config reads the whole running configuration.
func (c *CaddyClient) config(ctx context.Context) (config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/config/", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not read the Caddy configuration: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not read the Caddy configuration: Caddy answered with %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var cfg config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("could not read the Caddy configuration: %w", err)
	}

	return cfg, nil
}

// collectHostUpstreams records every upstream of the routes that serve one of
// the given hosts.
func collectHostUpstreams(node any, path string, hosts []string, into *[]Upstream) {
	switch n := node.(type) {
	case map[string]any:
		for _, key := range sortedKeys(n) {
			collectHostUpstreams(n[key], path+"/"+key, hosts, into)
		}
	case []any:
		for i, value := range n {
			child := fmt.Sprintf("%s/%d", path, i)

			if route, ok := value.(map[string]any); ok && matchesHost(route["match"], hosts) {
				collectUpstreams(route["handle"], child+"/handle", "", into)
			}

			collectHostUpstreams(value, child, hosts, into)
		}
	}
}

// collectUpstreams records the configuration path and the address of every
// reverse proxy upstream below node. An empty target records all of them.
func collectUpstreams(node any, path string, target string, into *[]Upstream) {
	switch n := node.(type) {
	case map[string]any:
		if handler, _ := n["handler"].(string); handler == "reverse_proxy" {
			for i, upstream := range slice(n["upstreams"]) {
				dial, _ := upstream.(map[string]any)["dial"].(string)
				if target != "" && dial != target {
					continue
				}

				*into = append(*into, Upstream{
					Path: fmt.Sprintf("%s/upstreams/%d/dial", path, i),
					Dial: dial,
				})
			}
		}

		for _, key := range sortedKeys(n) {
			collectUpstreams(n[key], path+"/"+key, target, into)
		}
	case []any:
		for i, value := range n {
			collectUpstreams(value, fmt.Sprintf("%s/%d", path, i), target, into)
		}
	}
}

// matchesHost reports whether a route matcher names one of the hosts.
func matchesHost(match any, hosts []string) bool {
	for _, entry := range slice(match) {
		matcher, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		for _, value := range slice(matcher["host"]) {
			name, _ := value.(string)

			for _, host := range hosts {
				if name == host {
					return true
				}
			}
		}
	}

	return false
}

// slice returns the value as a list, or an empty list when it is not one.
func slice(value any) []any {
	list, _ := value.([]any)
	return list
}

// sortedKeys returns the keys of a map in a stable order, so that the
// configuration paths do not depend on the order the map happens to have.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// portOf extracts the port from an upstream address such as "localhost:3001".
func portOf(dial string) (int, error) {
	idx := strings.LastIndex(dial, ":")
	if idx < 0 {
		return 0, fmt.Errorf("%q has no port", dial)
	}
	return strconv.Atoi(dial[idx+1:])
}

// FreePort returns a port nothing is listening on.
//
// The port is taken from the given range, which is expected to hold its
// first and last port. The port that is in use is skipped, and so is every
// port that cannot be bound.
func FreePort(portRange []int, inUse int) (int, error) {
	if len(portRange) != 2 {
		return 0, fmt.Errorf("the port range needs a first and a last port")
	}

	for port := portRange[0]; port < portRange[1]; port++ {
		if port == inUse {
			continue
		}

		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}

		listener.Close()

		return port, nil
	}

	return 0, fmt.Errorf("no free port in the range %d-%d", portRange[0], portRange[1])
}
