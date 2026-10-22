package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Status is the state of a deployment.
type Status string

const (
	// StatusPending means the deployment was accepted but has not started.
	StatusPending Status = "pending"
	// StatusRunning means the deployment is in progress.
	StatusRunning Status = "running"
	// StatusSucceeded means the deployment was published.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the deployment was abandoned.
	StatusFailed Status = "failed"
	// StatusRolledBack means the traffic was moved onto the new build and
	// then moved back again.
	StatusRolledBack Status = "rolled_back"
)

// Terminal reports whether the status is a final one.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusRolledBack
}

// Step is one stage of a deployment.
type Step struct {
	Name       string     `json:"name"`
	Status     Status     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Deployment is the record of one deployment.
//
// It is the answer to the question a publish workflow keeps asking: did this
// deployment go out, and if not, why not. The record is written while the
// deployment runs, so it can be read back at any moment — including while it
// is still running.
type Deployment struct {
	// ID identifies the deployment. It is the run id of the build.
	ID string `json:"id"`
	// App is the application that was deployed.
	App string `json:"app"`
	// RunID is the run the artifact was built by.
	RunID string `json:"run_id"`
	// Status is the state of the deployment.
	Status Status `json:"status"`
	// Reason explains a failure or a rollback.
	Reason string `json:"reason,omitempty"`
	// Warning explains something that went wrong but did not stop the
	// deployment.
	Warning string `json:"warning,omitempty"`
	// StartedAt is when the deployment began.
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is when the deployment reached a final state.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// ActivePort is the port the traffic was on before the deployment.
	ActivePort int `json:"active_port,omitempty"`
	// NewPort is the port the traffic was moved to.
	NewPort int `json:"new_port,omitempty"`
	// Steps are the stages of the deployment, in the order they ran.
	Steps []Step `json:"steps,omitempty"`
}

// Summary is the short form of a deployment, used when a list is served.
type Summary struct {
	ID         string     `json:"id"`
	App        string     `json:"app"`
	Status     Status     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Summary returns the short form of the deployment.
func (d *Deployment) Summary() Summary {
	return Summary{
		ID:         d.ID,
		App:        d.App,
		Status:     d.Status,
		Reason:     d.Reason,
		StartedAt:  d.StartedAt,
		FinishedAt: d.FinishedAt,
	}
}

// Store keeps the deployments of this host.
//
// The records survive a restart of the service: they are written to disk after
// every change, so a deployment that was interrupted is still known after the
// process comes back.
type Store struct {
	mu          sync.RWMutex
	deployments map[string]*Deployment
	order       []string
	dir         string
	max         int
}

// NewStore returns a store that keeps its records in dir.
func NewStore(dir string, max int) (*Store, error) {
	if max <= 0 {
		max = 50
	}

	s := &Store{
		deployments: map[string]*Deployment{},
		dir:         dir,
		max:         max,
	}

	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return nil, err
	}

	if err := s.load(); err != nil {
		return nil, err
	}

	return s, nil
}

// Dir returns the directory the records are kept in.
func (s *Store) Dir() string {
	return s.dir
}

// path is the file the records are kept in.
func (s *Store) path() string {
	return filepath.Join(s.dir, "deployments.json")
}

// logPath is the file the log of a deployment is kept in.
func (s *Store) logPath(app string, id string) string {
	return filepath.Join(s.dir, "logs", app, "run-"+id+".log")
}

// LogPath returns the file the log of a deployment is kept in.
func (s *Store) LogPath(app string, id string) string {
	return s.logPath(app, id)
}

// load reads the records that were written before.
func (s *Store) load() error {
	body, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var records []*Deployment
	if err := json.Unmarshal(body, &records); err != nil {
		return fmt.Errorf("could not read the deployment records: %w", err)
	}

	for _, record := range records {
		// A record that is already known is not added a second time.
		if _, exists := s.deployments[record.ID]; exists {
			continue
		}

		s.deployments[record.ID] = record
		s.order = append(s.order, record.ID)
	}

	return nil
}

// save writes the records to disk.
func (s *Store) save() {
	records := make([]*Deployment, 0, len(s.order))
	for _, id := range s.order {
		records = append(records, s.deployments[id])
	}

	body, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return
	}

	// The records are written to a temporary file and moved into place, so a
	// crash halfway through cannot leave a half-written file behind.
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		return
	}
}

// Add records a new deployment.
//
// A deployment that is recorded again — the same run id deployed a second
// time — replaces the record it had, so that the list does not hold it twice.
func (s *Store) Add(d *Deployment) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.deployments[d.ID]; !exists {
		s.order = append(s.order, d.ID)
	}

	s.deployments[d.ID] = d

	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.deployments, oldest)
	}

	s.save()
}

// Get returns a deployment by its id.
func (s *Store) Get(id string) (*Deployment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	d, ok := s.deployments[id]
	if !ok {
		return nil, false
	}

	clone := *d
	clone.Steps = append([]Step(nil), d.Steps...)

	return &clone, true
}

// List returns the summaries of the known deployments, newest first.
func (s *Store) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := make([]Summary, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if d, ok := s.deployments[s.order[i]]; ok {
			summaries = append(summaries, d.Summary())
		}
	}

	return summaries
}

// Update applies a change to a deployment and writes the records to disk.
func (s *Store) Update(id string, change func(*Deployment)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.deployments[id]
	if !ok {
		return
	}

	change(d)
	s.save()
}

// Step starts a new stage of a deployment.
func (s *Store) Step(id string, name string) {
	s.Update(id, func(d *Deployment) {
		d.Steps = append(d.Steps, Step{
			Name:      name,
			Status:    StatusRunning,
			StartedAt: time.Now().UTC(),
		})
	})
}

// FinishStep ends the stage that is currently running.
func (s *Store) FinishStep(id string, err error) {
	s.Update(id, func(d *Deployment) {
		for i := len(d.Steps) - 1; i >= 0; i-- {
			if d.Steps[i].Status != StatusRunning {
				continue
			}

			now := time.Now().UTC()
			d.Steps[i].FinishedAt = &now

			if err != nil {
				d.Steps[i].Status = StatusFailed
				d.Steps[i].Error = err.Error()
			} else {
				d.Steps[i].Status = StatusSucceeded
			}

			return
		}
	})
}

// Log reads the log of a deployment.
func (s *Store) Log(app string, id string) ([]byte, error) {
	return os.ReadFile(s.logPath(app, id))
}

// Prune removes the records that are older than the newest max deployments.
func (s *Store) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.deployments, oldest)
	}

	s.save()
}

// SortDeployments sorts deployments by their start time, newest first.
func SortDeployments(deployments []*Deployment) {
	sort.SliceStable(deployments, func(i, j int) bool {
		return deployments[i].StartedAt.After(deployments[j].StartedAt)
	})
}
