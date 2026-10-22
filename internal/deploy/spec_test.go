package deploy

import (
	"testing"
)

func TestExpand(t *testing.T) {
	vars := map[string]string{
		"port":    "3000",
		"run_id":  "12345678901",
		"missing": "",
	}

	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"{port}", "3000"},
		{"http://127.0.0.1:{port}/", "http://127.0.0.1:3000/"},
		{"{run_id}/{run_id}", "12345678901/12345678901"},
		{"", ""},
		// A placeholder without a value is left alone, so that a command
		// which legitimately contains braces survives.
		{"{unknown}", "{unknown}"},
		{"{unclosed", "{unclosed"},
		{"{port}{run_id}", "300012345678901"},
		// A pair of braces that means nothing is left alone, and the text
		// after it is not repeated.
		{"find . -exec rm -rf -- {} +", "find . -exec rm -rf -- {} +"},
		{"a {b} c {d} e", "a {b} c {d} e"},
		{"{port} {}", "3000 {}"},
	}

	for _, c := range cases {
		if got := expand(c.in, vars); got != c.want {
			t.Errorf("expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandAll(t *testing.T) {
	got := expandAll([]string{"curl", "-H", "Host: {app}", "http://127.0.0.1:{port}/"}, map[string]string{
		"app":  "example",
		"port": "3001",
	})

	want := []string{"curl", "-H", "Host: example", "http://127.0.0.1:3001/"}
	if len(got) != len(want) {
		t.Fatalf("expandAll returned %d arguments, want %d", len(got), len(want))
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argument %d is %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    *Spec
		wantErr bool
	}{
		{name: "nil", spec: nil, wantErr: true},
		{name: "no app", spec: &Spec{Workdir: "/tmp/x"}, wantErr: true},
		{name: "no workdir", spec: &Spec{App: "example"}, wantErr: true},
		{name: "minimal", spec: &Spec{App: "example", Workdir: "/tmp/x"}},
		{
			name:    "caddy without hosts",
			spec:    &Spec{App: "a", Workdir: "/tmp/x", Caddy: &Caddy{AdminURL: "http://localhost:2019"}},
			wantErr: true,
		},
		{
			name: "complete",
			spec: &Spec{App: "a", Workdir: "/tmp/x", Caddy: &Caddy{AdminURL: "http://localhost:2019", Hosts: []string{"a.example"}}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() returned %v, want an error: %v", err, c.wantErr)
			}
		})
	}
}

func TestSpecDialFor(t *testing.T) {
	spec := &Spec{App: "example", Caddy: &Caddy{AdminURL: "http://localhost:2019", Hosts: []string{"app.example"}}}

	if got := spec.dialFor(3001); got != "localhost:3001" {
		t.Errorf("dialFor(3001) = %q, want %q", got, "localhost:3001")
	}

	spec.Caddy.Dial = "127.0.0.1:{port}"
	if got := spec.dialFor(3000); got != "127.0.0.1:3000" {
		t.Errorf("dialFor(3000) = %q, want %q", got, "127.0.0.1:3000")
	}
}
