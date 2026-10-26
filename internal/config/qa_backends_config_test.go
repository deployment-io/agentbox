package config

import (
	"reflect"
	"strings"
	"testing"
)

const qaBackendsDeployed = `[{"repository":"1-acme/api","url":"https://d1.cloudfront.net","health_url":"https://d1.cloudfront.net/healthz"},` +
	`{"repository":"2-acme/worker","error":"The backend preview's deploy failed: the image build failed"}]`

func TestLoadQABackendsDeployedAndFailed(t *testing.T) {
	qaEnv(t, map[string]string{"QA_BACKENDS": qaBackendsDeployed})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	want := []QABackend{
		{Repository: "1-acme/api", URL: "https://d1.cloudfront.net", HealthURL: "https://d1.cloudfront.net/healthz"},
		{Repository: "2-acme/worker", Error: "The backend preview's deploy failed: the image build failed"},
	}
	if !reflect.DeepEqual(cfg.QABackends, want) {
		t.Errorf("QABackends = %+v, want %+v", cfg.QABackends, want)
	}
}

func TestLoadQABackendsRejectsBadEntries(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"not JSON", `[{"repository":`, "QA_BACKENDS must be a JSON array"},
		{"not an array", `{"repository":"1-acme/api"}`, "QA_BACKENDS must be a JSON array"},
		{"non-local repository", `[{"repository":"../etc","error":"x"}]`, `QA_BACKENDS entry 1 ("../etc")`},
		{"absolute repository", `[{"repository":"/work/1-acme/api","error":"x"}]`, `QA_BACKENDS entry 1 ("/work/1-acme/api")`},
		{"no repository", `[{"error":"x"}]`, `QA_BACKENDS entry 1 ("")`},
		{"http url", `[{"repository":"1-acme/api","url":"http://d1.cloudfront.net","health_url":"https://d1.cloudfront.net/healthz"}]`, `QA_BACKENDS entry 1 ("1-acme/api"): the url must be an https URL`},
		{"user info", `[{"repository":"1-acme/api","url":"https://u:p@d1.cloudfront.net","health_url":"https://d1.cloudfront.net/healthz"}]`, `QA_BACKENDS entry 1 ("1-acme/api"): the url must not carry user info`},
		{"other port", `[{"repository":"1-acme/api","url":"https://d1.cloudfront.net","health_url":"https://d1.cloudfront.net:8443/healthz"}]`, `QA_BACKENDS entry 1 ("1-acme/api"): the health_url must use port 443 or none`},
		{"url without health_url", `[{"repository":"1-acme/api","url":"https://d1.cloudfront.net"}]`, `QA_BACKENDS entry 1 ("1-acme/api"): the health_url is missing`},
		{"neither url nor error", `[{"repository":"1-acme/api"}]`, `QA_BACKENDS entry 1 ("1-acme/api") must have url and health_url, or error`},
		{"blank error", `[{"repository":"1-acme/api","error":"  "}]`, `QA_BACKENDS entry 1 ("1-acme/api") must have url and health_url, or error`},
		{"both url and error", `[{"repository":"1-acme/api","url":"https://d1.cloudfront.net","health_url":"https://d1.cloudfront.net/healthz","error":"x"}]`, `QA_BACKENDS entry 1 ("1-acme/api") must have either`},
		{"duplicate", `[{"repository":"1-acme/api","error":"x"},{"repository":"1-acme/api","error":"y"}]`, `QA_BACKENDS entry 2 ("1-acme/api") repeats repository`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qaEnv(t, map[string]string{"QA_BACKENDS": tc.raw})
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadQARepositoriesOptionalOnlyWithBackends(t *testing.T) {
	qaEnv(t, map[string]string{"QA_REPOSITORIES": "", "QA_BACKENDS": qaBackendsDeployed})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with backends and no repositories: %s", err)
	}
	if len(cfg.QARepositories) != 0 || len(cfg.QABackends) != 2 {
		t.Errorf("repositories %v, backends %v", cfg.QARepositories, cfg.QABackends)
	}

	for _, backends := range []string{"", "[]"} {
		qaEnv(t, map[string]string{"QA_REPOSITORIES": "", "QA_BACKENDS": backends})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QA_REPOSITORIES is required") {
			t.Errorf("QA_BACKENDS=%q and no repositories: err = %v", backends, err)
		}
	}
}

// QA_BACKENDS is a preview input: a production run never reads it, so it
// neither fills QABackends nor stands in for QA_REPOSITORIES.
func TestLoadQABackendsIgnoredForProduction(t *testing.T) {
	qaEnv(t, map[string]string{
		"QA_TARGET":       QATargetProduction,
		"QA_REPOSITORIES": "0-acme/website",
		"QA_URLS":         "0-acme/website=https://acme.example",
		"QA_BACKENDS":     "not even JSON",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.QABackends != nil {
		t.Errorf("QABackends = %+v, want none for production", cfg.QABackends)
	}

	qaEnv(t, map[string]string{
		"QA_TARGET":       QATargetProduction,
		"QA_REPOSITORIES": "",
		"QA_BACKENDS":     qaBackendsDeployed,
	})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QA_REPOSITORIES is required") {
		t.Errorf("production with backends and no repositories: err = %v", err)
	}
}
