// Package deploy implements the deployment pipeline behind webhook4deploy.
//
// A deployment is described by a Spec: where the artifact comes from, how it
// is unpacked, how the application is started, how its health is checked and
// how traffic is moved onto it. The Runner executes a Spec against a payload
// and records every step, so the outcome of a deployment can be read back over
// HTTP instead of being inferred from the state of the site.
//
// The package deliberately keeps the application's own knowledge in the Spec
// rather than in Go code. A Spec is data: it can be reviewed, it can change
// without a rebuild, and two applications that share a shape share a Spec.
package deploy

import (
	"fmt"
	"strconv"
	"strings"
)

// Source names where a deployment artifact is fetched from.
type Source string

const (
	// SourceR2 fetches the artifact from a Cloudflare R2 bucket.
	SourceR2 Source = "r2"
	// SourceGitHub fetches the artifact from a GitHub Actions artifact.
	SourceGitHub Source = "github"
)

// Health describes how the application is polled until it is ready to serve.
type Health struct {
	// URL of the endpoint to poll. "{port}" is replaced by the port the
	// application was started on.
	URL string `json:"url" yaml:"url"`
	// Attempts is the number of polls before the deployment is abandoned.
	Attempts int `json:"attempts,omitempty" yaml:"attempts,omitempty"`
	// IntervalSeconds is the pause between two polls.
	IntervalSeconds int `json:"interval-seconds,omitempty" yaml:"interval-seconds,omitempty"`
	// TimeoutSeconds bounds a single poll.
	TimeoutSeconds int `json:"timeout-seconds,omitempty" yaml:"timeout-seconds,omitempty"`
}

// Caddy describes how traffic is moved between the two ports an application
// is served on.
//
// The two ports exist so that a new build can be started, checked and only
// then take over the traffic. Caddy is told about the change through its
// admin API, which is a runtime change; the Persist commands write the same
// port into the Caddyfile so the change survives a restart of Caddy.
//
// Nothing here names a port. The tool reads the port the application is
// currently served on out of the running configuration of Caddy and picks a
// free one for the new build itself.
type Caddy struct {
	// AdminURL is the base URL of the Caddy admin API. It defaults to
	// http://localhost:2019.
	AdminURL string `json:"admin-url,omitempty" yaml:"admin-url,omitempty"`
	// Hosts are the host names whose routes serve the application. The
	// tool finds the upstreams of those routes in the running configuration
	// of Caddy and moves them onto the new build.
	Hosts []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	// Dial is the address the upstreams connect to. "{port}" is replaced by
	// the port the application was started on.
	Dial string `json:"dial,omitempty" yaml:"dial,omitempty"`
	// PortRange is the range a port for a new build is taken from. The first
	// free port in the range is used.
	PortRange []int `json:"port-range,omitempty" yaml:"port-range,omitempty"`
	// Persist are the commands that write the live port into the
	// configuration file of Caddy and reload it. The placeholders
	// "{active_port}" and "{port}" are available.
	Persist [][]string `json:"persist,omitempty" yaml:"persist,omitempty"`
}

// DefaultAdminURL is the address of the admin API of Caddy.
const DefaultAdminURL = "http://localhost:2019"

// DefaultPortRange is the range a port for a new build is taken from.
var DefaultPortRange = []int{7000, 7100}

// adminURL returns the address of the admin API of Caddy.
func (c *Caddy) adminURL() string {
	if c == nil || c.AdminURL == "" {
		return DefaultAdminURL
	}

	return c.AdminURL
}

// portRange returns the range a port for a new build is taken from.
func (c *Caddy) portRange() []int {
	if c == nil || len(c.PortRange) != 2 {
		return DefaultPortRange
	}

	return c.PortRange
}

// Check is one assertion about an endpoint.
//
// The assertions are about the status the endpoint answers with, because
// that is what says whether a request was routed where it should have been.
type Check struct {
	// URL of the endpoint to ask.
	URL string `json:"url" yaml:"url"`
	// Host is the Host header to send. Several sites share a listener and
	// are told apart by their host.
	Host string `json:"host,omitempty" yaml:"host,omitempty"`
	// Headers are the headers to send. The application reads some of them:
	// the protocol comes from X-Forwarded-Proto and the origin from the Host
	// header.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// Status is a list of the status codes the endpoint may answer with.
	Status []int `json:"status,omitempty" yaml:"status,omitempty"`
	// NotStatus is a list of the status codes the endpoint must not answer
	// with.
	NotStatus []int `json:"not-status,omitempty" yaml:"not-status,omitempty"`
	// TimeoutSeconds bounds the request.
	TimeoutSeconds int `json:"timeout-seconds,omitempty" yaml:"timeout-seconds,omitempty"`
}

// Expects reports whether the status satisfies the check. A check without an
// expectation is satisfied by any status below 500: the endpoint answered.
func (c Check) Expects(status int) bool {
	if len(c.Status) == 0 && len(c.NotStatus) == 0 {
		return status < 500
	}

	for _, allowed := range c.Status {
		if status == allowed {
			return true
		}
	}

	for _, forbidden := range c.NotStatus {
		if status == forbidden {
			return false
		}
	}

	return len(c.Status) == 0
}

// Spec describes how one application is deployed.
type Spec struct {
	// App is the name of the application. It names the deployment in the API
	// and the process that is managed.
	App string `json:"app" yaml:"app"`
	// Repo is the repository the artifact was built from. It is used for the
	// GitHub artifact fallback.
	Repo string `json:"repo,omitempty" yaml:"repo,omitempty"`
	// Source is where the artifact is fetched from when the payload does not
	// say.
	Source Source `json:"source,omitempty" yaml:"source,omitempty"`
	// ObjectKey is the key of the artifact in the bucket. "{run_id}" is
	// replaced by the run id of the deployment.
	ObjectKey string `json:"object-key,omitempty" yaml:"object-key,omitempty"`
	// Fetch is the command that downloads the artifact into "{dest}". The
	// placeholders "{bucket}", "{key}", "{dest}" and "{endpoint}" are
	// available. When it is empty the artifact is fetched from the bucket
	// with the AWS command line tool.
	Fetch []string `json:"fetch,omitempty" yaml:"fetch,omitempty"`
	// Workdir is the directory the application lives in. The build is
	// unpacked into a run-<id> directory below it.
	Workdir string `json:"workdir" yaml:"workdir"`

	// Install is the command that installs the dependencies of the unpacked
	// build. It is run in the run directory.
	Install []string `json:"install,omitempty" yaml:"install,omitempty"`
	// Stop are the commands that release the port before the new instance is
	// started. An application that is always served on the same port needs
	// them; an application that is moved onto a new port does not.
	Stop [][]string `json:"stop,omitempty" yaml:"stop,omitempty"`
	// Start is the command that starts the application. "{port}" is replaced
	// by the staging port and "{dir}" by the run directory.
	Start []string `json:"start,omitempty" yaml:"start,omitempty"`
	// Health describes how the started application is polled.
	Health *Health `json:"health,omitempty" yaml:"health,omitempty"`

	// Caddy describes how the traffic is moved onto the new build.
	Caddy *Caddy `json:"caddy,omitempty" yaml:"caddy,omitempty"`

	// VerifyRouting is run before anything is started, against the build that
	// is already live. These checks describe the routing configuration, not
	// the application, so a failure here is not the deployment's fault.
	VerifyRouting []Check `json:"verify-routing,omitempty" yaml:"verify-routing,omitempty"`
	// VerifyBuild is run against the new build on the staging port, before
	// the traffic is moved onto it. These checks describe the application.
	VerifyBuild []Check `json:"verify-build,omitempty" yaml:"verify-build,omitempty"`
	// VerifyLive is run through the proxy after the traffic has been moved.
	// These checks describe the application as it is served.
	VerifyLive []Check `json:"verify-live,omitempty" yaml:"verify-live,omitempty"`

	// Workers are the commands that start the background workers of the new
	// deployment, for example a token refresher.
	Workers [][]string `json:"workers,omitempty" yaml:"workers,omitempty"`
	// ProcessPrefix is the prefix of the processes of the application. The
	// processes of an older deployment are removed once the new deployment is
	// live, so that the two do not run side by side.
	ProcessPrefix string `json:"process-prefix,omitempty" yaml:"process-prefix,omitempty"`
	// Finalize are the commands that run after the deployment succeeded.
	Finalize [][]string `json:"finalize,omitempty" yaml:"finalize,omitempty"`
}

// Validate reports whether the Spec can be executed.
func (s *Spec) Validate() error {
	if s == nil {
		return fmt.Errorf("no deployment specification")
	}
	if s.App == "" {
		return fmt.Errorf("the deployment specification has no app")
	}
	if s.Workdir == "" {
		return fmt.Errorf("the deployment specification for %q has no workdir", s.App)
	}
	if s.Caddy != nil {
		if len(s.Caddy.Hosts) == 0 {
			return fmt.Errorf("the deployment specification for %q does not name the hosts it is served on", s.App)
		}
	}
	return nil
}

// dialFor renders the upstream address for a port.
func (s *Spec) dialFor(port int) string {
	dial := "localhost:{port}"
	if s.Caddy != nil && s.Caddy.Dial != "" {
		dial = s.Caddy.Dial
	}
	return expand(dial, map[string]string{"port": strconv.Itoa(port)})
}

// expand replaces every "{name}" placeholder with its value. A placeholder
// without a value is left alone, so a command that legitimately contains
// braces survives.
func expand(s string, vars map[string]string) string {
	var b strings.Builder

	for {
		start := strings.Index(s, "{")
		if start < 0 {
			b.WriteString(s)
			return b.String()
		}

		end := strings.Index(s[start:], "}")
		if end < 0 {
			b.WriteString(s)
			return b.String()
		}

		name := s[start+1 : start+end]
		value, ok := vars[name]
		if !ok {
			// A placeholder without a value is left alone, so that a command
			// which legitimately contains braces survives.
			b.WriteString(s[:start+end+1])
			s = s[start+end+1:]
			continue
		}

		b.WriteString(s[:start])
		b.WriteString(value)
		s = s[start+end+1:]
	}
}

// expandAll expands every element of a command.
func expandAll(command []string, vars map[string]string) []string {
	out := make([]string, len(command))
	for i, arg := range command {
		out[i] = expand(arg, vars)
	}
	return out
}
