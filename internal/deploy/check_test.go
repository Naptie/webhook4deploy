package deploy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAskDoesNotFollowRedirects is the reason a check that expects a
// redirect works: following the redirect would answer with whatever the
// redirect points at, and the check would say nothing about the routing.
func TestAskDoesNotFollowRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	status, err := ask(context.Background(), Check{}, server.URL+"/redirect")
	if err != nil {
		t.Fatalf("ask returned %v", err)
	}

	if status != http.StatusMovedPermanently {
		t.Errorf("ask returned %d, want %d", status, http.StatusMovedPermanently)
	}
}

// TestAskSendsTheHost: several sites share a listener and are told apart by
// their host.
func TestAskSendsTheHost(t *testing.T) {
	var seen string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if _, err := ask(context.Background(), Check{Host: "app.example"}, server.URL+"/"); err != nil {
		t.Fatalf("ask returned %v", err)
	}

	if seen != "app.example" {
		t.Errorf("the request carried the host %q, want %q", seen, "app.example")
	}
}

func TestCheckExpects(t *testing.T) {
	cases := []struct {
		name   string
		check  Check
		status int
		want   bool
	}{
		{
			name:   "no expectation takes anything below 500",
			check:  Check{},
			status: 200,
			want:   true,
		},
		{
			name:   "no expectation does not take a server error",
			check:  Check{},
			status: 502,
			want:   false,
		},
		{
			name:   "an allowed status",
			check:  Check{Status: []int{200, 201}},
			status: 201,
			want:   true,
		},
		{
			name:   "a status that is not allowed",
			check:  Check{Status: []int{200}},
			status: 301,
			want:   false,
		},
		{
			name:   "a forbidden status",
			check:  Check{NotStatus: []int{301, 302}},
			status: 302,
			want:   false,
		},
		{
			name:   "a status that is not forbidden",
			check:  Check{NotStatus: []int{301, 302}},
			status: 404,
			want:   true,
		},
		{
			name:   "both lists",
			check:  Check{Status: []int{200}, NotStatus: []int{500}},
			status: 200,
			want:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.check.Expects(c.status); got != c.want {
				t.Errorf("Expects(%d) = %v, want %v", c.status, got, c.want)
			}
		})
	}
}
