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
