package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.5.0", true},
		{"0.4.0", "0.4.1", true},
		{"0.9.0", "0.10.0", true},
		{"0.4.0", "1.0.0", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.1", "1.0.0", false},
		{"2.0.0", "1.9.9", false},
		// A build after a release is not older than it.
		{"0.4.0-1-gedbb50b-dirty", "0.4.0", false},
		{"0.4.0-1-gedbb50b-dirty", "0.4.1", true},
		{"dev", "1.0.0", false},
		{"1.0.0", "garbage", false},
		{"", "1.0.0", false},
	} {
		if got := Older(c.a, c.b); got != c.want {
			t.Errorf("Older(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSoftware(t *testing.T) {
	for in, want := range map[string]string{
		"Outloud 0.4.0":                  "0.4.0",
		"Outloud 0.4.0-1-gedbb50b-dirty": "0.4.0-1-gedbb50b-dirty",
		"Outloud dev":                    "",
		"":                               "",
		"Something 1.0.0":                "",
	} {
		if got := Software(in); got != want {
			t.Errorf("Software(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLatest(t *testing.T) {
	answer := `{"tag_name":"v0.5.0","html_url":"https://github.com/thomasreichmann/outloud/releases/tag/v0.5.0","name":"Outloud 0.5.0"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "GitHub wants a User-Agent", http.StatusForbidden)
			return
		}
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	defer func(u string) { latestURL = u }(latestURL)
	latestURL = srv.URL

	r, err := Latest(context.Background())
	if err != nil || r.Version != "0.5.0" || r.URL != "https://github.com/thomasreichmann/outloud/releases/tag/v0.5.0" {
		t.Errorf("got %+v, %v", r, err)
	}
	answer = `{"tag_name":"nightly"}`
	if r, err := Latest(context.Background()); err == nil {
		t.Errorf("took %+v", r)
	}
}

// TestLatestFromGitHub asks the real GitHub, where the latest release has
// to read; skipped without a network.
func TestLatestFromGitHub(t *testing.T) {
	if testing.Short() {
		t.Skip("asks GitHub")
	}
	r, err := Latest(context.Background())
	if err != nil {
		t.Skip(err)
	}
	if Older(r.Version, "0.4.0") {
		t.Errorf("the latest release is %+v", r)
	}
}
