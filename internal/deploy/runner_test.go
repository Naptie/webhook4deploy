package deploy

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePayload(t *testing.T) {
	payload, err := ParsePayload(map[string]any{
		"run_id":     "12345678901",
		"source":     "r2",
		"object_key": "example/12345678901/build.zip",
	})
	if err != nil {
		t.Fatalf("ParsePayload returned %v", err)
	}

	if payload.RunID != "12345678901" {
		t.Errorf("the run id is %q, want %q", payload.RunID, "12345678901")
	}

	if payload.Source != SourceR2 {
		t.Errorf("the source is %q, want %q", payload.Source, SourceR2)
	}

	if payload.ObjectKey != "example/12345678901/build.zip" {
		t.Errorf("the object key is %q", payload.ObjectKey)
	}
}

func TestParsePayloadWithoutARunID(t *testing.T) {
	if _, err := ParsePayload(map[string]any{"source": "r2"}); err == nil {
		t.Fatal("ParsePayload did not report that the request carries no run id")
	}

	if _, err := ParsePayload(nil); err == nil {
		t.Fatal("ParsePayload did not report that the request carries no run id")
	}
}

func TestUnpack(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "build.zip")

	writeArchive(t, archive, map[string]string{
		"build/index.js":      "console.log(1)",
		"package.json":        "{}",
		"scripts/run.sh":      "#!/bin/sh",
		"nested/dir/file.txt": "hello",
	})

	destination := filepath.Join(dir, "run-1")
	if err := unpack(archive, destination); err != nil {
		t.Fatalf("unpack returned %v", err)
	}

	body, err := os.ReadFile(filepath.Join(destination, "build", "index.js"))
	if err != nil {
		t.Fatalf("the file was not unpacked: %v", err)
	}

	if string(body) != "console.log(1)" {
		t.Errorf("the file holds %q", string(body))
	}

	if _, err := os.Stat(filepath.Join(destination, "nested", "dir", "file.txt")); err != nil {
		t.Errorf("the nested file was not unpacked: %v", err)
	}
}

func TestUnpackRefusesToEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "build.zip")

	writeArchive(t, archive, map[string]string{
		"../escaped.txt": "nope",
	})

	destination := filepath.Join(dir, "run-1")
	if err := unpack(archive, destination); err == nil {
		t.Fatal("unpack did not refuse a path that escapes the deployment directory")
	}
}

// writeArchive writes a zip archive with the given files.
func writeArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()

	handle, err := os.Create(path)
	if err != nil {
		t.Fatalf("the archive could not be created: %v", err)
	}
	defer handle.Close()

	writer := zip.NewWriter(handle)

	for name, body := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("the file %s could not be added: %v", name, err)
		}

		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatalf("the file %s could not be written: %v", name, err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("the archive could not be closed: %v", err)
	}
}
