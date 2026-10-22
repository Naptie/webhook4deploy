package deploy

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreAddAndGet(t *testing.T) {
	store, err := NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&Deployment{
		ID:        "12345678901",
		App:       "example",
		RunID:     "12345678901",
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
	})

	record, ok := store.Get("12345678901")
	if !ok {
		t.Fatal("the deployment was not found")
	}

	if record.App != "example" {
		t.Errorf("the deployment is for %q, want %q", record.App, "example")
	}

	if _, ok := store.Get("nothing"); ok {
		t.Error("a deployment that does not exist was found")
	}
}

func TestStoreList(t *testing.T) {
	store, err := NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&Deployment{ID: "1", App: "a", Status: StatusSucceeded, StartedAt: time.Now().UTC()})
	store.Add(&Deployment{ID: "2", App: "b", Status: StatusFailed, StartedAt: time.Now().UTC()})

	list := store.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d deployments, want 2", len(list))
	}

	// The newest deployment comes first.
	if list[0].ID != "2" {
		t.Errorf("the first deployment is %q, want %q", list[0].ID, "2")
	}
}

func TestStoreRecordsADeploymentOnce(t *testing.T) {
	store, err := NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&Deployment{ID: "1", App: "a", Status: StatusRunning, StartedAt: time.Now().UTC()})
	store.Add(&Deployment{ID: "1", App: "a", Status: StatusSucceeded, StartedAt: time.Now().UTC()})

	list := store.List()
	if len(list) != 1 {
		t.Fatalf("the list holds %d deployments, want 1: %v", len(list), list)
	}

	if list[0].Status != StatusSucceeded {
		t.Errorf("the deployment is %q, want %q", list[0].Status, StatusSucceeded)
	}
}

func TestStoreKeepsTheNewest(t *testing.T) {
	store, err := NewStore(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	for _, id := range []string{"1", "2", "3"} {
		store.Add(&Deployment{ID: id, App: "a", Status: StatusSucceeded, StartedAt: time.Now().UTC()})
	}

	if _, ok := store.Get("1"); ok {
		t.Error("the oldest deployment was not dropped")
	}

	if _, ok := store.Get("3"); !ok {
		t.Error("the newest deployment was dropped")
	}
}

func TestStoreSteps(t *testing.T) {
	store, err := NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&Deployment{ID: "1", App: "a", Status: StatusRunning, StartedAt: time.Now().UTC()})

	store.Step("1", "install")
	store.FinishStep("1", nil)

	store.Step("1", "health")
	store.FinishStep("1", errFailed)

	record, ok := store.Get("1")
	if !ok {
		t.Fatal("the deployment was not found")
	}

	if len(record.Steps) != 2 {
		t.Fatalf("the deployment has %d steps, want 2", len(record.Steps))
	}

	if record.Steps[0].Name != "install" || record.Steps[0].Status != StatusSucceeded {
		t.Errorf("the first step is %+v, want a succeeded install", record.Steps[0])
	}

	if record.Steps[1].Status != StatusFailed {
		t.Errorf("the second step is %q, want %q", record.Steps[1].Status, StatusFailed)
	}

	if record.Steps[1].Error == "" {
		t.Error("the failed step does not say what went wrong")
	}
}

func TestStoreSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	store, err := NewStore(dir, 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	store.Add(&Deployment{ID: "1", App: "example", Status: StatusSucceeded, StartedAt: time.Now().UTC()})

	// A second store reads the records that were written by the first one.
	restarted, err := NewStore(dir, 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	record, ok := restarted.Get("1")
	if !ok {
		t.Fatal("the deployment did not survive the restart")
	}

	if record.App != "example" {
		t.Errorf("the deployment is for %q, want %q", record.App, "example")
	}
}

func TestStoreLogPath(t *testing.T) {
	store, err := NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("NewStore returned %v", err)
	}

	path := store.LogPath("example", "12345678901")
	want := filepath.Join(store.Dir(), "logs", "example", "run-12345678901.log")

	if path != want {
		t.Errorf("LogPath is %q, want %q", path, want)
	}
}

// errFailed is the error a failed step reports.
var errFailed = &stepError{}

type stepError struct{}

func (e *stepError) Error() string { return "the step failed" }
