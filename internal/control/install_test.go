package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeInstaller struct{ installs int }

func (f *fakeInstaller) CheckDatabase(context.Context, DatabaseInput) (DatabaseCheck, error) {
	return DatabaseCheck{Version: "16"}, nil
}

func (f *fakeInstaller) Install(context.Context, InstallRequest, string) (InstallResult, error) {
	f.installs++
	return InstallResult{ConsoleURL: "https://argus.example.com"}, nil
}

func installServer(inst *fakeInstaller) http.Handler {
	return NewInstallHandler(InstallOptions{Token: "the-token", TokenFile: "/var/lib/argus-control/setup-token",
		Port: 8080, Installer: inst})
}

// With no configuration, every page goes to /setup and the API says why,
// rather than a control plane that would not start at all.
func TestInstallModeSendsEverythingToSetup(t *testing.T) {
	h := installServer(&fakeInstaller{})
	for path, want := range map[string]int{
		"/": http.StatusFound, "/assets": http.StatusFound, "/settings": http.StatusFound,
		"/api/v1/assets": http.StatusServiceUnavailable, "/healthz": http.StatusOK,
		"/auth/me": http.StatusUnauthorized, "/auth/setup": http.StatusOK,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if want == http.StatusFound && rec.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s went to %q", path, rec.Header().Get("Location"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/setup", nil))
	if !strings.Contains(rec.Body.String(), `"mode":"install"`) {
		t.Errorf("status does not say install: %s", rec.Body)
	}
}

// The token is checked before anything else, so neither the database test
// nor the install itself answers anyone who merely found the page.
func TestInstallNeedsTheToken(t *testing.T) {
	inst := &fakeInstaller{}
	h := installServer(inst)
	post := func(path, body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}
	if c := post("/auth/setup/database", `{"token":"guess","database":{"host":"db"}}`); c != http.StatusUnauthorized {
		t.Errorf("database check without the token: %d", c)
	}
	if c := post("/auth/setup/install", `{"token":"guess"}`); c != http.StatusUnauthorized || inst.installs != 0 {
		t.Errorf("install without the token: %d, %d installs", c, inst.installs)
	}
	if c := post("/auth/setup/install", `{"token":" the-token "}`); c != http.StatusCreated || inst.installs != 1 {
		t.Errorf("install with the token: %d, %d installs", c, inst.installs)
	}
}

func TestDatabaseInputURL(t *testing.T) {
	u, err := DatabaseInput{Host: "db.internal", Name: "argus", User: "argus", Password: "p@ss/word"}.URL()
	if err != nil {
		t.Fatal(err)
	}
	if u != "postgres://argus:p%40ss%2Fword@db.internal:5432/argus?sslmode=require" {
		t.Errorf("URL = %s", u)
	}
	back, ok := ParseDatabaseURL(u)
	if !ok || back.Host != "db.internal" || back.Port != 5432 || back.SSLMode != "require" {
		t.Errorf("round trip: %+v", back)
	}
	for _, bad := range []DatabaseInput{{}, {Host: "db", User: "u"}, {Host: "a/b", Name: "n", User: "u"},
		{Host: "db", Name: "n", User: "u", SSLMode: "sometimes"}} {
		if _, err := bad.URL(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestInstallValidate(t *testing.T) {
	in := InstallRequest{ConsoleURL: "https://argus.example.com:8080"}
	in.Admin.Email, in.Admin.Password = "you@example.com", "long-enough-password"
	if err := in.Validate(false); err != nil {
		t.Fatal(err)
	}
	if in.Port != 8080 {
		t.Errorf("port default %d", in.Port)
	}
	plain := in
	plain.ConsoleURL = "http://argus.example.com"
	if plain.Validate(false) == nil {
		t.Error("a plain-HTTP console address was accepted")
	}
	short := in
	short.Admin.Password = "short"
	if short.Validate(false) == nil {
		t.Error("a short password was accepted")
	}
	// A database that already has accounts keeps them; no administrator needed.
	if short.Validate(true) != nil {
		t.Error("an install keeping existing accounts still asked for an administrator")
	}
	half := in
	half.CertPEM = "x"
	if half.Validate(false) == nil {
		t.Error("a certificate without its key was accepted")
	}
}
