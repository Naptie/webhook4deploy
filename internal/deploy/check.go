package deploy

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// runChecks performs the assertions of a deployment.
//
// The assertions are about the status an endpoint answers with, because that
// is what says whether a request was routed where it should have been. A
// check that cannot be asked at all is a failure: an endpoint that does not
// answer is not an endpoint that works.
func (r *Runner) runChecks(ctx context.Context, name string, checks []Check, vars map[string]string, commands *CommandRunner) error {
	for i, check := range checks {
		url := expand(check.URL, vars)

		status, err := ask(ctx, check, url)
		if err != nil {
			commands.logf("\n=== %s %d: %s -> %v\n", name, i, url, err)
			return fmt.Errorf("%s check %d (%s) could not be made: %w", name, i, url, err)
		}

		commands.logf("\n=== %s %d: %s -> %d\n", name, i, url, status)

		if !check.Expects(status) {
			return fmt.Errorf("%s check %d (%s) answered with %d", name, i, url, status)
		}
	}

	return nil
}

// checkClient is the client the checks are made with.
//
// It does not follow redirects. A check that expects a redirect has to see
// the redirect itself: following it would answer with whatever the redirect
// points at, and the check would say nothing about the routing.
var checkClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ask makes the request of a check and returns the status it answered with.
func ask(ctx context.Context, check Check, url string) (int, error) {
	timeout := time.Duration(check.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	// The Host header is what tells the sites apart, several of them share
	// a listener.
	if check.Host != "" {
		req.Host = check.Host
	}

	for name, value := range check.Headers {
		req.Header.Set(name, value)
	}

	resp, err := checkClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}
