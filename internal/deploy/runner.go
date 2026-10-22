package deploy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Payload is what a caller asked to deploy.
type Payload struct {
	// RunID identifies the build.
	RunID string
	// App is the application the payload belongs to. It is filled in by the
	// runner from the specification.
	App string
	// Source is where the artifact is fetched from.
	Source Source
	// ObjectKey is the key of the artifact in the bucket.
	ObjectKey string
}

// ParsePayload reads the payload of a webhook request.
//
// The payload is the same one the publish workflow sends today: a run id, the
// source and the key of the artifact.
func ParsePayload(payload map[string]any) (Payload, error) {
	p := Payload{}

	if value, ok := payload["run_id"].(string); ok {
		p.RunID = value
	}
	if value, ok := payload["source"].(string); ok {
		p.Source = Source(value)
	}
	if value, ok := payload["object_key"].(string); ok {
		p.ObjectKey = value
	}

	if p.RunID == "" {
		return p, fmt.Errorf("the request does not carry a run_id")
	}

	return p, nil
}

// Runner executes deployments.
type Runner struct {
	// Store keeps the records of the deployments.
	Store *Store
	// Home is the home directory of the user the applications belong to. It
	// is the home directory of the commands.
	Home string
	// Env is the environment the commands are run in. It holds the
	// credentials the artifact is fetched with.
	Env map[string]string
}

// Run deploys an application and records what happened.
//
// The deployment runs in its own goroutine so that the caller can answer the
// request straight away: the outcome is read back over HTTP, not waited for.
func (r *Runner) Run(ctx context.Context, spec *Spec, payload Payload) {
	id := payload.RunID
	payload.App = spec.App

	deployment := &Deployment{
		ID:        id,
		App:       spec.App,
		RunID:     payload.RunID,
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
	}
	r.Store.Add(deployment)

	logFile, err := OpenLog(r.Store.LogPath(spec.App, id))
	if err != nil {
		r.Store.Update(id, func(d *Deployment) {
			d.Status = StatusFailed
			d.Reason = fmt.Sprintf("the log file could not be opened: %v", err)
			now := time.Now().UTC()
			d.FinishedAt = &now
		})
		return
	}
	defer logFile.Close()

	commands := &CommandRunner{Env: r.environment(), Log: logFile}

	fmt.Fprintf(logFile, "=== deployment %s of %s started at %s\n", id, spec.App, deployment.StartedAt.Format(time.RFC3339))

	err = r.pipeline(ctx, spec, payload, deployment, commands)

	now := time.Now().UTC()
	r.Store.Update(id, func(d *Deployment) {
		d.FinishedAt = &now
		if err == nil {
			d.Status = StatusSucceeded
			return
		}
		d.Status = StatusFailed
		d.Reason = err.Error()
	})

	fmt.Fprintf(logFile, "=== deployment %s finished at %s: %v\n", now.Format(time.RFC3339), id, err)
}

// pipeline runs the stages of a deployment.
func (r *Runner) pipeline(ctx context.Context, spec *Spec, payload Payload, d *Deployment, commands *CommandRunner) error {
	runDir := filepath.Join(spec.Workdir, "run-"+payload.RunID)

	// The artifact is fetched into a temporary file and unpacked into the run
	// directory. Everything the deployment needs afterwards is inside it.
	artifact, err := r.fetch(ctx, spec, payload, commands)
	if err != nil {
		return err
	}
	defer os.Remove(artifact)

	r.Store.Step(d.ID, "unpack")
	if err := unpack(artifact, runDir); err != nil {
		r.Store.FinishStep(d.ID, err)
		return err
	}
	r.Store.FinishStep(d.ID, nil)

	if len(spec.Install) > 0 {
		r.Store.Step(d.ID, "install")
		if _, err := commands.Run(ctx, "install", expandAll(spec.Install, r.vars(payload, runDir, 0)), runDir); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	// The routing is checked before anything is started. These commands
	// describe the configuration of the proxy, not the application, so a
	// failure here is not the fault of the deployment.
	if len(spec.VerifyRouting) > 0 {
		r.Store.Step(d.ID, "verify-routing")
		if err := r.runChecks(ctx, "verify-routing", spec.VerifyRouting, r.vars(payload, runDir, 0), commands); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	// The routes that serve the application are found in the running
	// configuration of the proxy, and the port they dial is the port the
	// application is live on. A free port is picked for the new build, so
	// the configuration never has to name one.
	activePort, newPort := 0, 0
	var caddy *CaddyClient
	var upstreamPaths []string

	if spec.Caddy != nil {
		caddy = NewCaddyClient(spec.Caddy.adminURL())

		r.Store.Step(d.ID, "read-the-routing")
		upstreams, err := caddy.UpstreamsForHosts(ctx, spec.Caddy.Hosts)
		if err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		if len(upstreams) == 0 {
			err := fmt.Errorf("none of the routes of %v serve an upstream", spec.Caddy.Hosts)
			r.Store.FinishStep(d.ID, err)
			return err
		}

		activePort, err = portOf(upstreams[0].Dial)
		if err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}

		// Every upstream that dials the same address serves the same
		// application, even when the route does not name it.
		upstreamPaths, err = caddy.UpstreamsWithDial(ctx, upstreams[0].Dial)
		if err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}

		newPort, err = FreePort(spec.Caddy.portRange(), activePort)
		if err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)

		r.Store.Update(d.ID, func(dep *Deployment) {
			dep.ActivePort = activePort
			dep.NewPort = newPort
		})
	}

	// The port the application is served on is released before the new
	// instance is started. An application that is moved onto a new port does
	// not need this; an application that always keeps its port does.
	//
	// A command that fails here is a warning rather than a failure: the
	// process may simply not be running. If the port is in fact still taken,
	// the new instance will not come up and the health check will say so.
	if len(spec.Stop) > 0 {
		r.Store.Step(d.ID, "stop")
		for i, command := range spec.Stop {
			if _, err := commands.Run(ctx, fmt.Sprintf("stop-%d", i), expandAll(command, r.vars(payload, runDir, newPort)), spec.Workdir); err != nil {
				r.warn(d.ID, fmt.Sprintf("the old process could not be stopped: %v", err))
			}
		}
		r.Store.FinishStep(d.ID, nil)
	}

	if len(spec.Start) > 0 {
		r.Store.Step(d.ID, "start")
		if _, err := commands.Run(ctx, "start", expandAll(spec.Start, r.vars(payload, runDir, newPort)), runDir); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	if spec.Health != nil && spec.Health.URL != "" {
		r.Store.Step(d.ID, "health")
		if err := waitHealthy(ctx, spec.Health, r.vars(payload, runDir, newPort)); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	// The new build is checked on the staging port, before the traffic is
	// moved onto it. A build that cannot serve is rejected while production
	// is still untouched.
	if len(spec.VerifyBuild) > 0 {
		r.Store.Step(d.ID, "verify-build")
		if err := r.runChecks(ctx, "verify-build", spec.VerifyBuild, r.vars(payload, runDir, newPort), commands); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	swapped := false

	if caddy != nil {
		r.Store.Step(d.ID, "switch")
		if err := caddy.Switch(ctx, upstreamPaths, spec.dialFor(newPort)); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		swapped = true
		r.Store.FinishStep(d.ID, nil)
	}

	// The one check that needs the traffic to have been moved: it catches a
	// build that answers on its own port but not as it is served.
	if len(spec.VerifyLive) > 0 {
		r.Store.Step(d.ID, "verify-live")
		if err := r.runChecks(ctx, "verify-live", spec.VerifyLive, r.vars(payload, runDir, newPort), commands); err != nil {
			r.Store.FinishStep(d.ID, err)
			r.rollback(ctx, d.ID, spec, caddy, upstreamPaths, activePort, newPort, commands)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	// The port is written into the configuration file of the proxy, so the
	// deployment survives a restart of the proxy.
	if spec.Caddy != nil && len(spec.Caddy.Persist) > 0 {
		r.Store.Step(d.ID, "persist")
		vars := r.vars(payload, runDir, newPort)
		vars["active_port"] = fmt.Sprintf("%d", activePort)

		var persistErr error
		for i, command := range spec.Caddy.Persist {
			if _, err := commands.Run(ctx, fmt.Sprintf("persist-%d", i), expandAll(command, vars), spec.Workdir); err != nil {
				persistErr = err
				break
			}
		}

		if persistErr != nil {
			// The deployment is published, but it will not survive a restart
			// of the proxy. That is a warning, not a failure.
			r.Store.FinishStep(d.ID, nil)
			r.Store.Update(d.ID, func(dep *Deployment) {
				dep.Warning = fmt.Sprintf("the live port could not be written into the configuration of the proxy: %v", persistErr)
			})
		} else {
			r.Store.FinishStep(d.ID, nil)
		}
	}

	// The processes of the older deployments are removed once the new
	// deployment is live. The processes of the new deployment are kept.
	if spec.ProcessPrefix != "" {
		r.Store.Step(d.ID, "remove-the-old-processes")
		if err := r.removeStaleProcesses(ctx, spec, payload.RunID, commands); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	for i, worker := range spec.Workers {
		r.Store.Step(d.ID, fmt.Sprintf("worker-%d", i))
		if _, err := commands.Run(ctx, fmt.Sprintf("worker-%d", i), expandAll(worker, r.vars(payload, runDir, newPort)), runDir); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	for i, command := range spec.Finalize {
		r.Store.Step(d.ID, fmt.Sprintf("finalize-%d", i))
		if _, err := commands.Run(ctx, fmt.Sprintf("finalize-%d", i), expandAll(command, r.vars(payload, runDir, newPort)), spec.Workdir); err != nil {
			r.Store.FinishStep(d.ID, err)
			return err
		}
		r.Store.FinishStep(d.ID, nil)
	}

	if swapped {
		fmt.Fprintf(commands.Log, "\n=== the traffic is on port %d now\n", newPort)
	}

	return nil
}

// removeStaleProcesses deletes the processes that belong to an older
// deployment of the application.
//
// The processes of the deployment that is running now are kept. Everything
// else that carries the prefix of the application is from a build that is no
// longer served.
func (r *Runner) removeStaleProcesses(ctx context.Context, spec *Spec, runID string, commands *CommandRunner) error {
	keep := spec.ProcessPrefix + "-" + runID

	result, err := commands.Run(ctx, "list-processes", []string{"pm2", "jlist"}, spec.Workdir)
	if err != nil {
		return err
	}

	var processes []struct {
		Name string `json:"name"`
	}

	if err := json.Unmarshal([]byte(result.Output), &processes); err != nil {
		return fmt.Errorf("the running processes could not be read: %w", err)
	}

	var stale []string
	for _, process := range processes {
		if !strings.HasPrefix(process.Name, spec.ProcessPrefix+"-") {
			continue
		}
		if strings.HasPrefix(process.Name, keep) {
			continue
		}

		stale = append(stale, process.Name)
	}

	for _, name := range stale {
		if _, err := commands.Run(ctx, "remove-process", []string{"pm2", "del", name}, spec.Workdir); err != nil {
			r.warn(runID, fmt.Sprintf("the process %s could not be removed: %v", name, err))
		}
	}

	return nil
}

// warn records a warning on a deployment.
func (r *Runner) warn(id string, message string) {
	r.Store.Update(id, func(d *Deployment) {
		if d.Warning == "" {
			d.Warning = message
			return
		}

		d.Warning = d.Warning + "; " + message
	})
}

// rollback moves the traffic back to the port it was on before.
func (r *Runner) rollback(ctx context.Context, id string, spec *Spec, caddy *CaddyClient, paths []string, activePort int, newPort int, commands *CommandRunner) {
	if caddy == nil || len(paths) == 0 {
		return
	}

	fmt.Fprintf(commands.Log, "\n=== rolling the traffic back to port %d\n", activePort)

	if err := caddy.Switch(ctx, paths, spec.dialFor(activePort)); err != nil {
		fmt.Fprintf(commands.Log, "=== the rollback failed: %v\n", err)
		r.Store.Update(id, func(d *Deployment) {
			d.Warning = fmt.Sprintf("the traffic could not be moved back to port %d: %v", activePort, err)
		})
	}
}

// fetch downloads the artifact of a deployment.
func (r *Runner) fetch(ctx context.Context, spec *Spec, payload Payload, commands *CommandRunner) (string, error) {
	key := payload.ObjectKey
	if key == "" {
		key = expand(spec.ObjectKey, map[string]string{"run_id": payload.RunID})
	}

	source := payload.Source
	if source == "" {
		source = spec.Source
	}

	// An artifact that is fetched from a bucket needs a key. One that is
	// fetched from a GitHub Actions run is found by its run id, but how it is
	// fetched is not something the tool knows: the helper that does it lives
	// on the host, so the configuration has to say.
	if key == "" && source != SourceGitHub {
		return "", fmt.Errorf("the deployment does not say where the artifact is")
	}

	if source == SourceGitHub && len(spec.Fetch) == 0 {
		return "", fmt.Errorf("the deployment does not say how to fetch the artifact from a GitHub Actions run")
	}

	destination := artifactPath(spec, source, payload.RunID)

	vars := map[string]string{
		"run_id": payload.RunID,
		"app":    spec.App,
		"repo":   spec.Repo,
		"key":    key,
		"dest":   destination,
	}
	for name, value := range r.Env {
		vars[name] = value
	}

	command := expandAll(spec.fetchCommand(), vars)

	r.Store.Step(payload.RunID, "fetch")
	if _, err := commands.Run(ctx, "fetch", command, spec.Workdir); err != nil {
		r.Store.FinishStep(payload.RunID, err)
		return "", err
	}
	r.Store.FinishStep(payload.RunID, nil)

	return destination, nil
}

// artifactPath is where the artifact of a deployment is downloaded to.
//
// The artifact of a GitHub Actions run is fetched by a helper that decides
// where to put it, so the path has to be the one the helper uses.
func artifactPath(spec *Spec, source Source, runID string) string {
	if source == SourceGitHub {
		return filepath.Join(os.TempDir(), filepath.FromSlash(spec.Repo), "build.zip")
	}

	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s-build.zip", spec.App, runID))
}

// fetchCommand is the command that downloads the artifact.
func (s *Spec) fetchCommand() []string {
	if len(s.Fetch) > 0 {
		return s.Fetch
	}

	return []string{"aws", "s3", "cp", "s3://{bucket}/{key}", "{dest}", "--endpoint-url", "{endpoint}"}
}

// vars returns the placeholders that are available to the commands.
func (r *Runner) vars(payload Payload, runDir string, port int) map[string]string {
	vars := map[string]string{
		"run_id": payload.RunID,
		"app":    payload.App,
		"dir":    runDir,
		"port":   fmt.Sprintf("%d", port),
	}

	for name, value := range r.Env {
		vars[name] = value
	}

	return vars
}

// environment returns the environment the commands are run in.
func (r *Runner) environment() []string {
	env := Environment(r.Home)

	for name, value := range r.Env {
		env = append(env, name+"="+value)
	}

	return env
}

// unpack extracts a zip archive into a directory.
func unpack(archive string, destination string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("the artifact could not be opened: %w", err)
	}
	defer reader.Close()

	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}

	for _, file := range reader.File {
		target := filepath.Join(destination, file.Name)

		// The archive is written by the build, but a path that escapes the
		// destination is refused rather than followed.
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destination)+string(os.PathSeparator)) {
			return fmt.Errorf("the artifact contains a path that escapes the deployment directory: %s", file.Name)
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		if err := extractFile(file, target); err != nil {
			return err
		}
	}

	return nil
}

// extractFile writes one file of the archive to disk.
func extractFile(file *zip.File, target string) error {
	source, err := file.Open()
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.Mode()|0o644)
	if err != nil {
		return err
	}
	defer destination.Close()

	if _, err := io.Copy(destination, source); err != nil {
		return err
	}

	return nil
}

// waitHealthy polls an endpoint until it answers.
func waitHealthy(ctx context.Context, health *Health, vars map[string]string) error {
	attempts := health.Attempts
	if attempts <= 0 {
		attempts = 20
	}

	interval := time.Duration(health.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}

	timeout := time.Duration(health.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	url := expand(health.URL, vars)

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := probe(ctx, url, timeout); err == nil {
			return nil
		} else if attempt == attempts {
			return fmt.Errorf("the application did not answer on %s after %d attempts: %w", url, attempts, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	return nil
}

// probe asks an endpoint whether it is ready.
func probe(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("the endpoint answered with %s", resp.Status)
	}

	return nil
}
