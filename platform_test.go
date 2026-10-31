package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func fixed(v string) *PlatformSpec { return &PlatformSpec{Mode: "fixed", Value: v} }

// A service's own platform wins over the project's, in both directions; nil inherits.
func TestPlatformArgsServiceOverridesProject(t *testing.T) {
	const multi = "--platform linux/amd64,linux/arm64"
	for _, tc := range []struct {
		name    string
		project PlatformSpec
		svc     *PlatformSpec
		want    string
	}{
		{"inherit auto", PlatformSpec{Mode: "auto"}, nil, multi},
		{"inherit fixed", *fixed("linux/arm64"), nil, "--platform linux/arm64"},
		{"service fixed over project auto", PlatformSpec{Mode: "auto"}, fixed("linux/amd64"), "--platform linux/amd64"},
		{"service fixed over project fixed", *fixed("linux/arm64"), fixed("linux/amd64"), "--platform linux/amd64"},
		{"service auto over project fixed", *fixed("linux/arm64"), &PlatformSpec{Mode: "auto"}, multi},
		{"unset project, service fixed", PlatformSpec{}, fixed("linux/amd64"), "--platform linux/amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(platformArgs(Descriptor{Platform: tc.project}, ServiceSpec{Name: "x", Platform: tc.svc}), " ")
			if got != tc.want {
				t.Fatalf("platformArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

// The override reaches the buildx argument vector of THAT service only.
func TestBuildxArgsPlatformIsPerService(t *testing.T) {
	d := Descriptor{Platform: PlatformSpec{Mode: "auto"}, Services: []ServiceSpec{
		{Name: "api"}, {Name: "browser", Platform: fixed("linux/amd64")},
	}}
	args := func(s ServiceSpec) string {
		return strings.Join(buildxArgs("b", d, s, "reg/"+s.Name, s.dockerfile(), "v", "bust", "", provenance{}), " ")
	}
	if got := args(d.Services[0]); !strings.Contains(got, "--platform linux/amd64,linux/arm64") {
		t.Errorf("api must stay multi-arch: %q", got)
	}
	got := args(d.Services[1])
	if !strings.Contains(got, "--platform linux/amd64 ") || strings.Contains(got, "arm64") {
		t.Errorf("browser must be amd64 only: %q", got)
	}
}

func TestDescriptorPlatformValidation(t *testing.T) {
	svc := func(p *PlatformSpec) Descriptor { return Descriptor{Services: []ServiceSpec{{Name: "s", Platform: p}}} }
	for _, tc := range []struct {
		name string
		d    Descriptor
		ok   bool
	}{
		{"every shape a deployed build.json uses: auto", Descriptor{Platform: PlatformSpec{Mode: "auto"}}, true},
		{"fixed arm64", Descriptor{Platform: *fixed("linux/arm64")}, true},
		{"project mode unset means auto", Descriptor{}, true},
		{"fixed list", Descriptor{Platform: *fixed("linux/amd64,linux/arm64")}, true},
		{"variant", Descriptor{Platform: *fixed("linux/arm/v7")}, true},
		{"service fixed", svc(fixed("linux/amd64")), true},
		{"service auto", svc(&PlatformSpec{Mode: "auto"}), true},
		{"project mode typo", Descriptor{Platform: PlatformSpec{Mode: "fixd", Value: "linux/amd64"}}, false},
		{"project fixed without value", Descriptor{Platform: PlatformSpec{Mode: "fixed"}}, false},
		{"project fixed bad value", Descriptor{Platform: *fixed("amd64")}, false},
		{"service fixed without value", svc(&PlatformSpec{Mode: "fixed"}), false},
		{"service empty mode is ambiguous", svc(&PlatformSpec{}), false},
		{"service mode typo", svc(&PlatformSpec{Mode: "amd64"}), false},
		{"service bad value", svc(fixed("windows/amd64")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.d.validate(); (err == nil) != tc.ok {
				t.Fatalf("validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestServicePlatformDecodesFromBuildJSON(t *testing.T) {
	var d Descriptor
	if err := json.Unmarshal([]byte(`{"project":"p","platform":{"mode":"auto"},"services":[{"name":"a"},{"name":"b","platform":{"mode":"fixed","value":"linux/amd64"}}]}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Services[0].Platform != nil || d.Services[1].Platform == nil || d.Services[1].Platform.Value != "linux/amd64" {
		t.Fatalf("decoded %+v", d.Services)
	}
}

// A bad platform is refused at the trigger with 400, before any build starts.
func TestTriggerRefusesAnInvalidPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/builds", (&app{}).handleTriggerBuild)
	body := `{"descriptor":{"project":"p","services":[{"name":"s","platform":{"mode":"fixed"}}]},"registry":"r","repo":"x"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/builds", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "service s: platform.value") {
		t.Fatalf("trigger = %d %s, want 400 naming the service", w.Code, w.Body)
	}
}

// Release gate, run by hand against every build.json in the tree: BUILD_JSON_LIST is a file of paths.
// Skipped when unset.
func TestEveryDeployedBuildJSONStillValidates(t *testing.T) {
	list := os.Getenv("BUILD_JSON_LIST")
	if list == "" {
		t.Skip("BUILD_JSON_LIST not set")
	}
	raw, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, path := range strings.Fields(string(raw)) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var d Descriptor
		if err := json.Unmarshal(b, &d); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if err := d.validate(); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		n++
	}
	t.Logf("validated %d build.json files", n)
}
