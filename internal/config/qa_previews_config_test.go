package config

import (
	"reflect"
	"strings"
	"testing"
)

const qaPreviewsValid = `[` +
	`{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api"],"unconnected_backends":[]},` +
	`{"repository":"2-acme/admin","kind":"site","url":"https://d3.cloudfront.net","unconnected_backends":["1-acme/api"]},` +
	`{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"},` +
	`{"repository":"1-acme/api","kind":"site","url":"https://d4.cloudfront.net","connected_backends":["1-acme/api"]},` +
	`{"repository":"3-acme/docs","kind":"site","error":"the build failed"},` +
	`{"repository":"4-acme/worker","kind":"backend","error":"the image build failed"}]`

// qaPreviewsEnv is a preview QA run given QA_PREVIEWS: the runner sends
// QA_REPOSITORIES and QA_BACKENDS blank with it.
func qaPreviewsEnv(t *testing.T, previews string) {
	t.Helper()
	qaEnv(t, map[string]string{"QA_REPOSITORIES": "", "QA_PREVIEWS": previews})
}

func TestLoadQAPreviews(t *testing.T) {
	qaPreviewsEnv(t, qaPreviewsValid)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	want := []QAPreview{
		{Repository: "0-acme/web", Kind: QAPreviewSite, URL: "https://d1.cloudfront.net", ConnectedBackends: []string{"1-acme/api"}},
		{Repository: "2-acme/admin", Kind: QAPreviewSite, URL: "https://d3.cloudfront.net", UnconnectedBackends: []string{"1-acme/api"}},
		{Repository: "1-acme/api", Kind: QAPreviewBackend, URL: "https://d2.cloudfront.net", HealthURL: "https://d2.cloudfront.net/healthz"},
		{Repository: "1-acme/api", Kind: QAPreviewSite, URL: "https://d4.cloudfront.net", ConnectedBackends: []string{"1-acme/api"}},
		{Repository: "3-acme/docs", Kind: QAPreviewSite, Error: "the build failed"},
		{Repository: "4-acme/worker", Kind: QAPreviewBackend, Error: "the image build failed"},
	}
	if !reflect.DeepEqual(cfg.QAPreviews, want) {
		t.Errorf("QAPreviews = %+v\nwant %+v", cfg.QAPreviews, want)
	}
	if len(cfg.QARepositories) != 0 || len(cfg.QABackends) != 0 {
		t.Errorf("repositories %v, backends %v, want none", cfg.QARepositories, cfg.QABackends)
	}
}

func TestLoadQAPreviewsRejectsBadEntries(t *testing.T) {
	const api = `{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"}`
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"not JSON", `[{"repository":`, "QA_PREVIEWS must be a JSON array"},
		{"not an array", `{"repository":"0-acme/web"}`, "QA_PREVIEWS must be a JSON array"},
		{"not objects", `["0-acme/web"]`, "QA_PREVIEWS must be a JSON array"},
		{"no repository", `[{"kind":"site","error":"x"}]`, `QA_PREVIEWS entry 1 ("") must name a repository`},
		{"non-local repository", `[{"repository":"../etc","kind":"site","error":"x"}]`, `QA_PREVIEWS entry 1 ("../etc") must name a repository`},
		{"absolute repository", `[{"repository":"/work/0-acme/web","kind":"site","error":"x"}]`, `QA_PREVIEWS entry 1 ("/work/0-acme/web") must name a repository`},
		{"no kind", `[{"repository":"0-acme/web","error":"x"}]`, `QA_PREVIEWS entry 1 ("0-acme/web") has kind ""`},
		{"bad kind", `[{"repository":"0-acme/web","kind":"worker","error":"x"}]`, `QA_PREVIEWS entry 1 ("0-acme/web") has kind "worker"`},
		{"repeated", `[{"repository":"0-acme/web","kind":"site","error":"x"},{"repository":"0-acme/web","kind":"site","error":"y"}]`, `QA_PREVIEWS entry 2 ("0-acme/web") repeats the site preview`},
		{"both url and error", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","error":"x"}]`, `QA_PREVIEWS entry 1 ("0-acme/web") must have either url or error, not both`},
		{"health_url and error", `[{"repository":"1-acme/api","kind":"backend","health_url":"https://d2.cloudfront.net/healthz","error":"x"}]`, `QA_PREVIEWS entry 1 ("1-acme/api") must have either url or error, not both`},
		{"neither", `[{"repository":"0-acme/web","kind":"site"}]`, `QA_PREVIEWS entry 1 ("0-acme/web") must have url or error`},
		{"blank error", `[{"repository":"0-acme/web","kind":"site","error":" "}]`, `QA_PREVIEWS entry 1 ("0-acme/web") must have url or error`},
		{"http url", `[{"repository":"0-acme/web","kind":"site","url":"http://d1.cloudfront.net"}]`, `QA_PREVIEWS entry 1 ("0-acme/web"): the url must be an https URL`},
		{"user info", `[{"repository":"0-acme/web","kind":"site","url":"https://u:p@d1.cloudfront.net"}]`, `QA_PREVIEWS entry 1 ("0-acme/web"): the url must not carry user info`},
		{"bad health_url", `[{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net:8443/healthz"}]`, `QA_PREVIEWS entry 1 ("1-acme/api"): the health_url must use port 443 or none`},
		{"backend without health_url", `[{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net"}]`, `QA_PREVIEWS entry 1 ("1-acme/api"): the health_url is missing`},
		{"site with health_url", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","health_url":"https://d1.cloudfront.net/healthz"}]`, `QA_PREVIEWS entry 1 ("0-acme/web") is a site, so it must not have a health_url`},
		{"backend with connected_backends", `[` + api + `,{"repository":"2-acme/jobs","kind":"backend","url":"https://d5.cloudfront.net","health_url":"https://d5.cloudfront.net/healthz","connected_backends":["1-acme/api"]}]`, `QA_PREVIEWS entry 2 ("2-acme/jobs") is a backend, so it must not have connected_backends`},
		{"failed backend with unconnected_backends", `[` + api + `,{"repository":"2-acme/jobs","kind":"backend","error":"x","unconnected_backends":["1-acme/api"]}]`, `QA_PREVIEWS entry 2 ("2-acme/jobs") failed to deploy, so it must not have`},
		{"failed site with connected_backends", `[` + api + `,{"repository":"0-acme/web","kind":"site","error":"x","connected_backends":["1-acme/api"]}]`, `QA_PREVIEWS entry 2 ("0-acme/web") failed to deploy, so it must not have`},
		{"failed site with unconnected_backends", `[` + api + `,{"repository":"0-acme/web","kind":"site","error":"x","unconnected_backends":["1-acme/api"]}]`, `QA_PREVIEWS entry 2 ("0-acme/web") failed to deploy, so it must not have`},
		{"failed site with empty connected_backends", `[` + api + `,{"repository":"0-acme/web","kind":"site","error":"x","connected_backends":[]}]`, `QA_PREVIEWS entry 2 ("0-acme/web") failed to deploy, so it must not have`},
		{"failed site with null unconnected_backends", `[` + api + `,{"repository":"0-acme/web","kind":"site","error":"x","unconnected_backends":null}]`, `QA_PREVIEWS entry 2 ("0-acme/web") failed to deploy, so it must not have`},
		{"backend with empty unconnected_backends", `[` + api + `,{"repository":"2-acme/jobs","kind":"backend","url":"https://d5.cloudfront.net","health_url":"https://d5.cloudfront.net/healthz","unconnected_backends":[]}]`, `QA_PREVIEWS entry 2 ("2-acme/jobs") is a backend, so it must not have connected_backends`},
		{"failed backend with empty connected_backends", `[` + api + `,{"repository":"2-acme/jobs","kind":"backend","error":"x","connected_backends":[]}]`, `QA_PREVIEWS entry 2 ("2-acme/jobs") failed to deploy, so it must not have`},
		{"url not a string", `[` + api + `,{"repository":"0-acme/web","kind":"site","url":123}]`, `QA_PREVIEWS entry 2 ("0-acme/web") is not a valid preview entry`},
		{"list not strings", `[` + api + `,{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":[1]}]`, `QA_PREVIEWS entry 2 ("0-acme/web") is not a valid preview entry`},
		{"repository not a string", `[{"repository":7,"kind":"site","error":"x"}]`, `QA_PREVIEWS entry 1 ("") is not a valid preview entry`},
		{"null entry", `[` + api + `,null]`, "QA_PREVIEWS must be a JSON array of preview entries: entry 2 is not a JSON object"},
		{"connected to no backend entry", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api"]}]`, `QA_PREVIEWS entry 1 ("0-acme/web"): connected_backends names "1-acme/api", which has no backend entry`},
		{"connected to a site entry", `[{"repository":"1-acme/api","kind":"site","error":"x"},{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api"]}]`, `QA_PREVIEWS entry 2 ("0-acme/web"): connected_backends names "1-acme/api", which has no backend entry`},
		{"unconnected to no backend entry", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","unconnected_backends":["9-acme/gone"]},` + api + `]`, `QA_PREVIEWS entry 1 ("0-acme/web"): unconnected_backends names "9-acme/gone", which has no backend entry`},
		{"in both lists", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api"],"unconnected_backends":["1-acme/api"]},` + api + `]`, `QA_PREVIEWS entry 1 ("0-acme/web") names backend "1-acme/api" in both connected_backends and unconnected_backends`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qaPreviewsEnv(t, tc.raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// A backend named twice in the same list is not an error: it is listed once.
func TestLoadQAPreviewsRepeatedInOneList(t *testing.T) {
	qaPreviewsEnv(t, `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api"," 1-acme/api"]},`+
		`{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"}]`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if got := cfg.QAPreviews[0].ConnectedBackends; !reflect.DeepEqual(got, []string{"1-acme/api"}) {
		t.Errorf("ConnectedBackends = %q, want [1-acme/api]", got)
	}
}

func TestLoadQAPreviewsExcludesQARepositoriesAndQABackends(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"QA_REPOSITORIES": {"QA_REPOSITORIES": "0-acme/web", "QA_PREVIEWS": qaPreviewsValid},
		"QA_BACKENDS":     {"QA_REPOSITORIES": "", "QA_BACKENDS": qaBackendsDeployed, "QA_PREVIEWS": qaPreviewsValid},
	} {
		qaEnv(t, env)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QA_PREVIEWS must not be set together with QA_REPOSITORIES or QA_BACKENDS") {
			t.Errorf("QA_PREVIEWS with %s: err = %v", name, err)
		}
	}
}

// QA_PREVIEWS with an entry is something to check; an empty one is not.
func TestLoadQAPreviewsIsSomethingToCheck(t *testing.T) {
	qaPreviewsEnv(t, `[{"repository":"0-acme/web","kind":"site","error":"the build failed"}]`)
	if _, err := Load(); err != nil {
		t.Errorf("Load with only QA_PREVIEWS: %s", err)
	}
	for _, previews := range []string{"", "  ", "[]"} {
		qaPreviewsEnv(t, previews)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QA_REPOSITORIES is required") {
			t.Errorf("QA_PREVIEWS=%q and nothing else: err = %v", previews, err)
		}
	}
}

// QA_PREVIEWS is a preview input: a production run never reads it.
func TestLoadQAPreviewsIgnoredForProduction(t *testing.T) {
	qaEnv(t, map[string]string{
		"QA_TARGET":       QATargetProduction,
		"QA_REPOSITORIES": "0-acme/website",
		"QA_URLS":         "0-acme/website=https://acme.example",
		"QA_PREVIEWS":     "not even JSON",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.QAPreviews != nil {
		t.Errorf("QAPreviews = %+v, want none for production", cfg.QAPreviews)
	}

	qaEnv(t, map[string]string{"QA_TARGET": QATargetProduction, "QA_REPOSITORIES": "", "QA_PREVIEWS": qaPreviewsValid})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QA_REPOSITORIES is required") {
		t.Errorf("production with previews and no repositories: err = %v", err)
	}
}

// QA_API_REQUEST_TOOL turns verify_api_request on only when it is "1" and the
// target is the preview one.
func TestLoadQAAPIRequestTool(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"1", true}, {"", false}, {"0", false}, {"true", false}, {"yes", false}, {" 1 ", false}, {"1\n", false}} {
		qaEnv(t, map[string]string{"QA_REPOSITORIES": "", "QA_PREVIEWS": qaPreviewsValid, "QA_API_REQUEST_TOOL": tc.value})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %s", err)
		}
		if cfg.QAAPIRequestTool != tc.want {
			t.Errorf("QA_API_REQUEST_TOOL=%q: QAAPIRequestTool = %v", tc.value, cfg.QAAPIRequestTool)
		}
	}
	qaEnv(t, map[string]string{
		"QA_TARGET":           QATargetProduction,
		"QA_REPOSITORIES":     "0-acme/website",
		"QA_URLS":             "0-acme/website=https://acme.example",
		"QA_API_REQUEST_TOOL": "1",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.QAAPIRequestTool {
		t.Error("a production run has QAAPIRequestTool")
	}
}

// qaPreviewsWithRoles is a site and a backend entry whose url has roles.
func qaPreviewsWithRoles(roles string) string {
	return `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api"]},` +
		`{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz","roles":` + roles + `}]`
}

func TestLoadQAPreviewsRoles(t *testing.T) {
	qaPreviewsEnv(t, qaPreviewsWithRoles(`[{"name":"admin","description":"Manages every account"},{"name":"read_only-2","description":""},{"name":"member"}]`))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	want := []QARole{{Name: "admin", Description: "Manages every account"}, {Name: "read_only-2"}, {Name: "member"}}
	if got := cfg.QAPreviews[1].Roles; !reflect.DeepEqual(got, want) {
		t.Errorf("Roles = %+v, want %+v", got, want)
	}
	if cfg.QAPreviews[0].Roles != nil {
		t.Errorf("the site has roles %+v", cfg.QAPreviews[0].Roles)
	}

	// Ten roles, a 32-character name and a 200-character description are the limits.
	var ten []string
	for i := 0; i < 10; i++ {
		ten = append(ten, `{"name":"role`+string(rune('a'+i))+`"}`)
	}
	ten[0] = `{"name":"a` + strings.Repeat("b", 31) + `","description":"` + strings.Repeat("é", 200) + `"}`
	qaPreviewsEnv(t, qaPreviewsWithRoles(`[`+strings.Join(ten, ",")+`]`))
	if cfg, err := Load(); err != nil || len(cfg.QAPreviews[1].Roles) != 10 {
		t.Errorf("ten roles at the limits: err = %v", err)
	}
}

func TestLoadQAPreviewsRejectsBadRoles(t *testing.T) {
	const api = `{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"}`
	var eleven []string
	for i := 0; i < 11; i++ {
		eleven = append(eleven, `{"name":"role`+string(rune('a'+i))+`"}`)
	}
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"on a site entry", `[` + api + `,{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","roles":[{"name":"admin"}]}]`, `QA_PREVIEWS entry 2 ("0-acme/web") is a site, so it must not have roles`},
		{"empty on a site entry", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","roles":[]}]`, `QA_PREVIEWS entry 1 ("0-acme/web") is a site, so it must not have roles`},
		{"on a failed backend entry", `[{"repository":"1-acme/api","kind":"backend","error":"x","roles":[{"name":"admin"}]}]`, `QA_PREVIEWS entry 1 ("1-acme/api") failed to deploy, so it must not have roles`},
		{"on a failed site entry", `[{"repository":"0-acme/web","kind":"site","error":"x","roles":[{"name":"admin"}]}]`, `QA_PREVIEWS entry 1 ("0-acme/web") failed to deploy, so it must not have roles`},
		{"11 roles", qaPreviewsWithRoles(`[` + strings.Join(eleven, ",") + `]`), `QA_PREVIEWS entry 2 ("1-acme/api") has 11 roles: at most 10`},
		{"an upper-case name", qaPreviewsWithRoles(`[{"name":"Admin"}]`), `QA_PREVIEWS entry 2 ("1-acme/api"): role 1 has name "Admin": must match`},
		{"a name starting with a digit", qaPreviewsWithRoles(`[{"name":"admin"},{"name":"2fa"}]`), `QA_PREVIEWS entry 2 ("1-acme/api"): role 2 has name "2fa": must match`},
		{"a name with a space", qaPreviewsWithRoles(`[{"name":"super admin"}]`), `role 1 has name "super admin": must match`},
		{"a 33-character name", qaPreviewsWithRoles(`[{"name":"a` + strings.Repeat("b", 32) + `"}]`), `role 1 has name`},
		{"no name", qaPreviewsWithRoles(`[{"description":"x"}]`), `role 1 has name "": must match`},
		{"a duplicate", qaPreviewsWithRoles(`[{"name":"admin"},{"name":"member"},{"name":"admin"}]`), `QA_PREVIEWS entry 2 ("1-acme/api") repeats role "admin"`},
		{"a name with spaces around it", qaPreviewsWithRoles(`[{"name":" admin "}]`), `role 1 has name " admin ": must match`},
		{"a description long by a trailing space", qaPreviewsWithRoles(`[{"name":"admin","description":"` + strings.Repeat("x", 200) + ` "}]`), `role "admin" has a description of 201 characters: at most 200`},
		{"a long description", qaPreviewsWithRoles(`[{"name":"admin","description":"` + strings.Repeat("x", 201) + `"}]`), `QA_PREVIEWS entry 2 ("1-acme/api"): role "admin" has a description of 201 characters: at most 200`},
		{"not a list", qaPreviewsWithRoles(`"admin"`), `QA_PREVIEWS entry 2 ("1-acme/api") is not a valid preview entry`},
		{"a name not a string", qaPreviewsWithRoles(`[{"name":7}]`), `QA_PREVIEWS entry 2 ("1-acme/api") is not a valid preview entry`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qaPreviewsEnv(t, tc.raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// qaPreviewsWithBrowserRoles is a site connected to two backends, with
// browser_roles.
func qaPreviewsWithBrowserRoles(roles string) string {
	return `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","connected_backends":["1-acme/api","2-acme/auth"],"browser_roles":` + roles + `},` +
		`{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"},` +
		`{"repository":"2-acme/auth","kind":"backend","url":"https://d5.cloudfront.net","health_url":"https://d5.cloudfront.net/healthz"},` +
		`{"repository":"3-acme/admin","kind":"site","url":"https://d3.cloudfront.net","unconnected_backends":["1-acme/api"]}]`
}

func TestLoadQAPreviewsBrowserRoles(t *testing.T) {
	qaPreviewsEnv(t, qaPreviewsWithBrowserRoles(`[{"name":"owner","description":"Owns the workspace","backend":"1-acme/api"},{"name":"member","backend":"1-acme/api"},{"name":"owner","description":"","backend":" 2-acme/auth "}]`))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	want := []QABrowserRole{
		{Name: "owner", Description: "Owns the workspace", Backend: "1-acme/api"},
		{Name: "member", Backend: "1-acme/api"},
		{Name: "owner", Backend: "2-acme/auth"},
	}
	if got := cfg.QAPreviews[0].BrowserRoles; !reflect.DeepEqual(got, want) {
		t.Errorf("BrowserRoles = %+v, want %+v", got, want)
	}
	for _, p := range cfg.QAPreviews[1:] {
		if p.BrowserRoles != nil || p.Roles != nil {
			t.Errorf("%s has roles %+v %+v", p.Repository, p.Roles, p.BrowserRoles)
		}
	}

	// Ten roles per backend, a 32-character name and a 200-character description are the limits.
	var twenty []string
	for _, backend := range []string{"1-acme/api", "2-acme/auth"} {
		for i := 0; i < 10; i++ {
			twenty = append(twenty, `{"name":"role`+string(rune('a'+i))+`","backend":"`+backend+`"}`)
		}
	}
	twenty[0] = `{"name":"a` + strings.Repeat("b", 31) + `","description":"` + strings.Repeat("é", 200) + `","backend":"1-acme/api"}`
	qaPreviewsEnv(t, qaPreviewsWithBrowserRoles(`[`+strings.Join(twenty, ",")+`]`))
	if cfg, err := Load(); err != nil || len(cfg.QAPreviews[0].BrowserRoles) != 20 {
		t.Errorf("ten browser roles per backend at the limits: err = %v", err)
	}
}

func TestLoadQAPreviewsRejectsBadBrowserRoles(t *testing.T) {
	const api = `{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"}`
	var eleven []string
	for i := 0; i < 11; i++ {
		eleven = append(eleven, `{"name":"role`+string(rune('a'+i))+`","backend":"2-acme/auth"}`)
	}
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"on a backend entry", `[{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz","browser_roles":[{"name":"admin","backend":"1-acme/api"}]}]`, `QA_PREVIEWS entry 1 ("1-acme/api") is a backend, so it must not have browser_roles`},
		{"empty on a backend entry", `[{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz","browser_roles":[]}]`, `QA_PREVIEWS entry 1 ("1-acme/api") is a backend, so it must not have browser_roles`},
		{"on a failed site entry", `[` + api + `,{"repository":"0-acme/web","kind":"site","error":"x","browser_roles":[{"name":"admin","backend":"1-acme/api"}]}]`, `QA_PREVIEWS entry 2 ("0-acme/web") failed to deploy, so it must not have browser_roles`},
		{"on a failed backend entry", `[{"repository":"1-acme/api","kind":"backend","error":"x","browser_roles":[]}]`, `QA_PREVIEWS entry 1 ("1-acme/api") failed to deploy, so it must not have browser_roles`},
		{"a backend not connected", qaPreviewsWithBrowserRoles(`[{"name":"admin","backend":"1-acme/api"},{"name":"admin","backend":"9-acme/other"}]`), `QA_PREVIEWS entry 1 ("0-acme/web"): browser role 2 has backend "9-acme/other", which is not in its connected_backends`},
		{"no backend", qaPreviewsWithBrowserRoles(`[{"name":"admin"}]`), `browser role 1 has backend "", which is not in its connected_backends`},
		{"on a site without connected_backends", `[` + api + `,{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","unconnected_backends":["1-acme/api"],"browser_roles":[{"name":"admin","backend":"1-acme/api"}]}]`, `QA_PREVIEWS entry 2 ("0-acme/web"): browser role 1 has backend "1-acme/api", which is not in its connected_backends`},
		{"an upper-case name", qaPreviewsWithBrowserRoles(`[{"name":"Admin","backend":"1-acme/api"}]`), `QA_PREVIEWS entry 1 ("0-acme/web"): browser role 1 has name "Admin": must match`},
		{"no name", qaPreviewsWithBrowserRoles(`[{"backend":"1-acme/api"}]`), `browser role 1 has name "": must match`},
		{"a 33-character name", qaPreviewsWithBrowserRoles(`[{"name":"a` + strings.Repeat("b", 32) + `","backend":"1-acme/api"}]`), `browser role 1 has name`},
		{"a repeated (backend, name)", qaPreviewsWithBrowserRoles(`[{"name":"admin","backend":"1-acme/api"},{"name":"admin","backend":"2-acme/auth"},{"name":"admin","backend":"1-acme/api"}]`), `QA_PREVIEWS entry 1 ("0-acme/web") repeats browser role "admin" of backend "1-acme/api"`},
		{"11 roles of one backend", qaPreviewsWithBrowserRoles(`[` + strings.Join(eleven, ",") + `]`), `QA_PREVIEWS entry 1 ("0-acme/web") has 11 browser roles of backend "2-acme/auth": at most 10`},
		{"a long description", qaPreviewsWithBrowserRoles(`[{"name":"admin","description":"` + strings.Repeat("x", 201) + `","backend":"1-acme/api"}]`), `QA_PREVIEWS entry 1 ("0-acme/web"): browser role "admin" has a description of 201 characters: at most 200`},
		{"not a list", qaPreviewsWithBrowserRoles(`"admin"`), `QA_PREVIEWS entry 1 ("0-acme/web") is not a valid preview entry`},
		{"a backend not a string", qaPreviewsWithBrowserRoles(`[{"name":"admin","backend":1}]`), `QA_PREVIEWS entry 1 ("0-acme/web") is not a valid preview entry`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qaPreviewsEnv(t, tc.raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// qaPagesBackend is a deployed backend entry whose front page answered a web
// page, with the extra keys given.
func qaPagesBackend(extra string) string {
	return `{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz","serves_pages":true` + extra + `}`
}

func TestLoadQAPreviewsServesPages(t *testing.T) {
	qaPreviewsEnv(t, `[`+qaPagesBackend("")+`,{"repository":"2-acme/auth","kind":"backend","url":"https://d5.cloudfront.net","health_url":"https://d5.cloudfront.net/healthz","serves_pages":false}]`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if !cfg.QAPreviews[0].ServesPages || cfg.QAPreviews[1].ServesPages {
		t.Errorf("ServesPages = %v, %v, want true, false", cfg.QAPreviews[0].ServesPages, cfg.QAPreviews[1].ServesPages)
	}

	// A backend that serves pages may have browser roles of its own.
	qaPreviewsEnv(t, `[`+qaPagesBackend(`,"browser_roles":[{"name":"owner","description":"Owns the workspace","backend":" 1-acme/api "},{"name":"member","backend":"1-acme/api"}]`)+`]`)
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	want := []QABrowserRole{
		{Name: "owner", Description: "Owns the workspace", Backend: "1-acme/api"},
		{Name: "member", Backend: "1-acme/api"},
	}
	if got := cfg.QAPreviews[0].BrowserRoles; !reflect.DeepEqual(got, want) {
		t.Errorf("BrowserRoles = %+v, want %+v", got, want)
	}
}

func TestLoadQAPreviewsRejectsBadServesPages(t *testing.T) {
	const api = `{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz"}`
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"on a site entry", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","serves_pages":true}]`, `QA_PREVIEWS entry 1 ("0-acme/web") is a site, so it must not have serves_pages`},
		{"false on a site entry", `[{"repository":"0-acme/web","kind":"site","url":"https://d1.cloudfront.net","serves_pages":false}]`, `QA_PREVIEWS entry 1 ("0-acme/web") is a site, so it must not have serves_pages`},
		{"on a failed backend entry", `[{"repository":"1-acme/api","kind":"backend","error":"x","serves_pages":true}]`, `QA_PREVIEWS entry 1 ("1-acme/api") failed to deploy, so it must not have serves_pages`},
		{"false on a failed site entry", `[{"repository":"0-acme/web","kind":"site","error":"x","serves_pages":false}]`, `QA_PREVIEWS entry 1 ("0-acme/web") failed to deploy, so it must not have serves_pages`},
		{"not a bool", `[{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz","serves_pages":"yes"}]`, `QA_PREVIEWS entry 1 ("1-acme/api") is not a valid preview entry`},
		{"browser roles on a backend without serves_pages", `[{"repository":"1-acme/api","kind":"backend","url":"https://d2.cloudfront.net","health_url":"https://d2.cloudfront.net/healthz","serves_pages":false,"browser_roles":[{"name":"admin","backend":"1-acme/api"}]}]`, `QA_PREVIEWS entry 1 ("1-acme/api") is a backend, so it must not have browser_roles`},
		{"a browser role of another backend", `[` + qaPagesBackend(`,"browser_roles":[{"name":"admin","backend":"1-acme/api"},{"name":"admin","backend":"2-acme/auth"}]`) + `,{"repository":"2-acme/auth","kind":"backend","url":"https://d5.cloudfront.net","health_url":"https://d5.cloudfront.net/healthz"}]`, `QA_PREVIEWS entry 1 ("1-acme/api"): browser role 2 has backend "2-acme/auth", which is not its own repository`},
		{"a browser role with no backend", `[` + qaPagesBackend(`,"browser_roles":[{"name":"admin"}]`) + `]`, `QA_PREVIEWS entry 1 ("1-acme/api"): browser role 1 has backend "", which is not its own repository`},
		{"a repeated browser role", `[` + qaPagesBackend(`,"browser_roles":[{"name":"admin","backend":"1-acme/api"},{"name":"admin","backend":"1-acme/api"}]`) + `]`, `QA_PREVIEWS entry 1 ("1-acme/api") repeats browser role "admin" of backend "1-acme/api"`},
		{"a bad browser role name", `[` + api + `,{"repository":"2-acme/auth","kind":"backend","url":"https://d5.cloudfront.net","health_url":"https://d5.cloudfront.net/healthz","serves_pages":true,"browser_roles":[{"name":"Admin","backend":"2-acme/auth"}]}]`, `QA_PREVIEWS entry 2 ("2-acme/auth"): browser role 1 has name "Admin": must match`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qaPreviewsEnv(t, tc.raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	var eleven []string
	for i := 0; i < 11; i++ {
		eleven = append(eleven, `{"name":"role`+string(rune('a'+i))+`","backend":"1-acme/api"}`)
	}
	qaPreviewsEnv(t, `[`+qaPagesBackend(`,"browser_roles":[`+strings.Join(eleven, ",")+`]`)+`]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `QA_PREVIEWS entry 1 ("1-acme/api") has 11 browser roles of backend "1-acme/api": at most 10`) {
		t.Errorf("11 browser roles: err = %v", err)
	}
}

// QA_API_SIGN_IN_TOOL is read like QA_API_REQUEST_TOOL: only "1", only for
// the preview target.
func TestLoadQAAPISignInTool(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"1", true}, {"", false}, {"0", false}, {"true", false}, {" 1 ", false}} {
		qaEnv(t, map[string]string{"QA_REPOSITORIES": "", "QA_PREVIEWS": qaPreviewsWithRoles(`[{"name":"admin"}]`), "QA_API_REQUEST_TOOL": "1", "QA_API_SIGN_IN_TOOL": tc.value})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %s", err)
		}
		if cfg.QAAPISignInTool != tc.want {
			t.Errorf("QA_API_SIGN_IN_TOOL=%q: QAAPISignInTool = %v", tc.value, cfg.QAAPISignInTool)
		}
	}
	qaEnv(t, map[string]string{
		"QA_TARGET":           QATargetProduction,
		"QA_REPOSITORIES":     "0-acme/website",
		"QA_URLS":             "0-acme/website=https://acme.example",
		"QA_API_SIGN_IN_TOOL": "1",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.QAAPISignInTool {
		t.Error("a production run has QAAPISignInTool")
	}
}
