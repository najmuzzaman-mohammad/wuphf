package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nex-crm/wuphf/internal/provider"
)

// stubLookPath maps binary names to canned (path, err) results.
func stubLookPath(t *testing.T, results map[string]struct {
	path string
	err  error
}) func(string) (string, error) {
	t.Helper()
	return func(name string) (string, error) {
		r, ok := results[name]
		if !ok {
			return "", errors.New("not stubbed")
		}
		return r.path, r.err
	}
}

// TestComputeLocalProviderStatuses_AllInstalledAndReachable is the
// happy path: all binaries on PATH, all endpoints
// reachable, version + model are surfaced.
func TestComputeLocalProviderStatuses_AllInstalledAndReachable(t *testing.T) {
	ov := localProvidersStatusOverrides{
		lookPath: stubLookPath(t, map[string]struct {
			path string
			err  error
		}{
			"mlx_lm.server": {path: "/Users/x/.local/bin/mlx_lm.server"},
			"ollama":        {path: "/usr/local/bin/ollama"},
			"exo":           {path: "/Users/x/.local/bin/exo"},
			"hermes":        {path: "/Users/x/.local/bin/hermes"},
			"openclaw":      {path: "/usr/local/bin/openclaw"},
		}),
		runVer: func(_ context.Context, path string, _ []string) (string, error) {
			return "v1.2.3", nil
		},
		probe: func(_ context.Context, _ string, baseURL string) (bool, string, bool) {
			return true, "fake-loaded-model:" + baseURL, true
		},
		goos:   "darwin",
		goarch: "arm64",
	}
	t.Setenv("ATLASCLOUD_API_KEY", "atlas-test-key")
	got := computeLocalProviderStatuses(context.Background(), ov)
	if len(got) != 6 {
		t.Fatalf("expected 6 statuses, got %d", len(got))
	}
	gotByKind := map[string]LocalProviderStatus{}
	for _, s := range got {
		gotByKind[s.Kind] = s
	}
	for _, k := range []string{provider.KindAtlasCloud, provider.KindMLXLM, provider.KindOllama, provider.KindExo, provider.KindHermesAgent, provider.KindOpenclawHTTP} {
		s, ok := gotByKind[k]
		if !ok {
			t.Fatalf("missing kind %q", k)
		}
		if !s.BinaryInstalled {
			t.Errorf("%s: BinaryInstalled = false, want true", k)
		}
		if !s.Reachable {
			t.Errorf("%s: Reachable = false, want true", k)
		}
		if !s.PlatformSupported {
			t.Errorf("%s: PlatformSupported = false on darwin, want true", k)
		}
		if s.BinaryVersion == "" {
			t.Errorf("%s: BinaryVersion empty, version probe should have populated it", k)
		}
		if k != provider.KindAtlasCloud && !strings.HasPrefix(s.LoadedModel, "fake-loaded-model:") {
			t.Errorf("%s: LoadedModel = %q, want fake prefix", k, s.LoadedModel)
		}
	}
}

// TestComputeLocalProviderStatuses_NoneInstalled simulates a fresh
// machine: nothing on PATH, all kinds report
// BinaryInstalled=false but still surface install commands so the
// doctor panel can render them.
func TestComputeLocalProviderStatuses_NoneInstalled(t *testing.T) {
	ov := localProvidersStatusOverrides{
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		runVer:   func(_ context.Context, _ string, _ []string) (string, error) { return "", nil },
		probe:    func(_ context.Context, _, _ string) (bool, string, bool) { return false, "", true },
		goos:     "darwin",
		goarch:   "arm64",
	}
	got := computeLocalProviderStatuses(context.Background(), ov)
	for _, s := range got {
		if s.BinaryInstalled {
			t.Errorf("%s: BinaryInstalled = true on a fresh box", s.Kind)
		}
		if s.Kind != provider.KindAtlasCloud && len(s.Install) == 0 {
			t.Errorf("%s: Install map empty — UI has nothing to show", s.Kind)
		}
		if s.Kind == provider.KindAtlasCloud && len(s.Notes) == 0 {
			t.Errorf("atlascloud: Notes empty — UI has no API key setup hint")
		}
		if s.Endpoint == "" {
			t.Errorf("%s: Endpoint empty — UI can't render the URL", s.Kind)
		}
	}
}

// TestComputeLocalProviderStatuses_LinuxHidesMLXButShowsOllamaExo
// confirms the platform gate: MLX-LM is Apple Silicon only and must
// surface PlatformSupported=false on Linux. Ollama, Exo, Hermes, and
// OpenClaw Gateway stay supported.
func TestComputeLocalProviderStatuses_LinuxHidesMLXButShowsOthers(t *testing.T) {
	ov := localProvidersStatusOverrides{
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		runVer:   func(_ context.Context, _ string, _ []string) (string, error) { return "", nil },
		probe:    func(_ context.Context, _, _ string) (bool, string, bool) { return false, "", false },
		goos:     "linux",
	}
	got := computeLocalProviderStatuses(context.Background(), ov)
	for _, s := range got {
		switch s.Kind {
		case provider.KindMLXLM:
			if s.PlatformSupported {
				t.Errorf("mlx-lm: PlatformSupported = true on linux")
			}
		case provider.KindAtlasCloud, provider.KindOllama, provider.KindExo, provider.KindHermesAgent, provider.KindOpenclawHTTP:
			if !s.PlatformSupported {
				t.Errorf("%s: PlatformSupported = false on linux", s.Kind)
			}
		}
	}
}

// TestComputeLocalProviderStatuses_IntelMacRejectsMLXLM is the v7
// Major regression: marking every `darwin` host as MLX-supported
// sent Intel-Mac users to a UI that advertised the runtime as
// Running, even though MLX wheels don't load on x86_64. Now mlx-lm
// requires darwin+arm64; Intel Macs see PlatformSupported=false
// and the disabled-tile copy the wizard already renders for
// Linux/Windows.
func TestComputeLocalProviderStatuses_IntelMacRejectsMLXLM(t *testing.T) {
	ov := localProvidersStatusOverrides{
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		runVer:   func(_ context.Context, _ string, _ []string) (string, error) { return "", nil },
		probe:    func(_ context.Context, _, _ string) (bool, string, bool) { return false, "", false },
		goos:     "darwin",
		goarch:   "amd64", // Intel Mac
	}
	got := computeLocalProviderStatuses(context.Background(), ov)
	for _, s := range got {
		switch s.Kind {
		case provider.KindMLXLM:
			if s.PlatformSupported {
				t.Errorf("mlx-lm: PlatformSupported = true on darwin/amd64 (Intel Mac); MLX requires Apple Silicon")
			}
		case provider.KindAtlasCloud, provider.KindOllama, provider.KindExo, provider.KindHermesAgent, provider.KindOpenclawHTTP:
			// Ollama/Exo/Hermes/OpenClaw Gateway work on both arm64 and amd64
			// Intel Macs should still see those as supported.
			if !s.PlatformSupported {
				t.Errorf("%s: PlatformSupported = false on darwin/amd64", s.Kind)
			}
		}
	}
}

// TestComputeLocalProviderStatuses_WindowsAddsWSL2Note covers the
// Windows path: every kind is unsupported and gets a one-liner
// nudging the user toward WSL2.
func TestComputeLocalProviderStatuses_WindowsAddsWSL2Note(t *testing.T) {
	ov := localProvidersStatusOverrides{
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		runVer:   func(_ context.Context, _ string, _ []string) (string, error) { return "", nil },
		probe:    func(_ context.Context, _, _ string) (bool, string, bool) { return false, "", false },
		goos:     "windows",
	}
	got := computeLocalProviderStatuses(context.Background(), ov)
	for _, s := range got {
		if s.Kind == provider.KindAtlasCloud {
			if !s.PlatformSupported {
				t.Errorf("atlascloud: PlatformSupported = false on windows; cloud runtime should remain OS-independent")
			}
			continue
		}
		if s.PlatformSupported {
			t.Errorf("%s: PlatformSupported = true on windows; WSL2 path expected", s.Kind)
		}
		if !strings.Contains(strings.ToLower(s.WindowsNote), "wsl") {
			t.Errorf("%s: WindowsNote missing WSL hint: %q", s.Kind, s.WindowsNote)
		}
	}
}

// TestComputeLocalProviderStatuses_NonLoopbackEndpointSkipsProbe is
// the security guardrail test: a remote base URL set via env or
// config must NOT trigger an outbound probe from the broker. Probe
// is loopback-only.
func TestComputeLocalProviderStatuses_NonLoopbackEndpointSkipsProbe(t *testing.T) {
	t.Setenv("WUPHF_OLLAMA_BASE_URL", "http://10.0.0.99:11434/v1")

	// The probe must NEVER fire for the non-loopback ollama override.
	// mlx-lm and exo keep their loopback defaults so their probes are
	// fine — the assertion checks the URL, not the call count.
	ov := localProvidersStatusOverrides{
		lookPath: func(name string) (string, error) {
			if name == "ollama" {
				return "/usr/local/bin/ollama", nil
			}
			return "", errors.New("not found")
		},
		runVer: func(_ context.Context, _ string, _ []string) (string, error) { return "v1", nil },
		probe: func(_ context.Context, _ string, baseURL string) (bool, string, bool) {
			if strings.Contains(baseURL, "10.0.0.99") {
				t.Errorf("probe was called for non-loopback URL %q — security guardrail breached", baseURL)
			}
			return false, "", true
		},
		goos:   "darwin",
		goarch: "arm64",
	}
	got := computeLocalProviderStatuses(context.Background(), ov)
	for _, s := range got {
		if s.Kind != provider.KindOllama {
			continue
		}
		if s.Probed {
			t.Errorf("ollama Probed = true for non-loopback endpoint")
		}
		if s.Reachable {
			t.Errorf("ollama Reachable = true without probe")
		}
		if s.ProbeSkippedNote == "" {
			t.Errorf("ollama ProbeSkippedNote empty — UI has no explanation for the missing reachability dot")
		}
	}
}

// TestIsLoopbackBaseURL covers the security predicate directly. The
// test exists separately from the integration test above so a
// regression here is easy to localize.
func TestIsLoopbackBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://127.0.0.1:8080/v1", true},
		{"http://[::1]:11434/v1", true},
		{"http://localhost:8080", true},
		{"http://LocalHost:8080", true},  // case-insensitive (browsers do this)
		{"http://0.0.0.0:8080/v1", true}, // wildcard bind reachable from loopback
		{"http://10.0.0.5:8080", false},
		{"http://10.0.0.99:11434/v1", false}, // matches the security-guardrail test fixture
		{"http://[2001:db8::1]:8080/v1", false},
		{"https://api.example.com/v1", false},
		{"", false},
		{"::not a url", false},
		// Decimal-encoded IPs and similar obfuscation aren't resolved
		// here on purpose — net.ParseIP rejects "2130706433" so it
		// falls through to the false branch, which is correct: we
		// don't want a hostile config probing arbitrary hosts via
		// integer encoding.
		{"http://2130706433/v1", false},
	}
	for _, tc := range cases {
		if got := isLoopbackBaseURL(tc.in); got != tc.want {
			t.Errorf("isLoopbackBaseURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestProbeOpenAICompatEndpoint_Real exercises the production probe
// against a real httptest server, confirming the JSON shape parses
// and the first model id is surfaced.
func TestProbeOpenAICompatEndpoint_Real(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"qwen2.5-coder:7b"},{"id":"llama3"}]}`)
	}))
	defer srv.Close()

	reachable, model, ok := probeOpenAICompatEndpoint(context.Background(), provider.KindMLXLM, srv.URL)
	if !ok {
		t.Fatal("ok = false; probe didn't even reach the server")
	}
	if !reachable {
		t.Fatal("reachable = false against a 200 server")
	}
	if model != "qwen2.5-coder:7b" {
		t.Errorf("model = %q, want qwen2.5-coder:7b", model)
	}
}

func TestProbeOpenAICompatEndpoint_SendsRuntimeBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"openclaw/default"}]}`)
	}))
	defer srv.Close()

	t.Setenv("WUPHF_OPENCLAW_HTTP_API_KEY", "")
	t.Setenv("OPENCLAW_GATEWAY_TOKEN", "gateway-token")
	t.Setenv("WUPHF_OPENCLAW_TOKEN", "")
	reachable, model, ok := probeOpenAICompatEndpoint(context.Background(), provider.KindOpenclawHTTP, srv.URL)
	if !ok || !reachable {
		t.Fatalf("probe failed: ok=%v reachable=%v model=%q", ok, reachable, model)
	}
	if gotAuth != "Bearer gateway-token" {
		t.Fatalf("Authorization = %q, want gateway bearer token", gotAuth)
	}
}

// TestProbeOpenAICompatEndpoint_Non2xx confirms a 503 surfaces as
// reachable=false rather than the loaded-model field getting a junk
// HTML body.
func TestProbeOpenAICompatEndpoint_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	reachable, _, _ := probeOpenAICompatEndpoint(context.Background(), provider.KindMLXLM, srv.URL)
	if reachable {
		t.Errorf("503 should not count as reachable")
	}
}

// TestComputeLocalProviderStatuses_DocumentedSurface freezes the JSON
// field set so any Go-side rename (e.g. binary_installed →
// binaryInstalled) trips a backend test before it can drift the
// LocalProviderStatus interface in web/src/api/client.ts.
//
// The check decodes a marshalled status into map[string]json.RawMessage
// and asserts the key set equals the documented contract — both
// missing fields (rename slipped through) AND extra fields (a new
// optional field added without updating client.ts) fail loudly.
func TestComputeLocalProviderStatuses_DocumentedSurface(t *testing.T) {
	ov := localProvidersStatusOverrides{
		// Force every optional field to be populated so the marshalled
		// shape exercises the full surface.
		lookPath: func(string) (string, error) {
			return "/usr/local/bin/fake", nil
		},
		runVer: func(_ context.Context, _ string, _ []string) (string, error) {
			return "v1.2.3", nil
		},
		probe: func(_ context.Context, _, _ string) (bool, string, bool) {
			return true, "loaded-model-x", true
		},
		// Pick windows so windows_note + probe_skipped_note (when
		// platform_supported flips false) get exercised; we also need
		// a non-windows variant to confirm install/start populate, so
		// we run two passes below.
		goos: "windows",
	}

	// Documented field set. Every entry MUST appear in at least one
	// status payload across the runs below. Update this list when
	// LocalProviderStatus genuinely gains/loses a field — and when you
	// do, update web/src/api/client.ts in the same PR.
	expected := map[string]bool{
		"kind":               true,
		"binary_installed":   true,
		"binary_path":        true,
		"binary_version":     true,
		"endpoint":           true,
		"model":              true,
		"reachable":          true,
		"loaded_model":       true,
		"probed":             true,
		"probe_skipped_note": true,
		"platform_supported": true,
		"windows_note":       true,
		"install":            true,
		"start":              true,
		"notes":              true,
	}

	seen := map[string]bool{}
	collect := func(payload []LocalProviderStatus) {
		for _, item := range payload {
			body, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(body, &keys); err != nil {
				t.Fatalf("unmarshal back: %v\nbody=%s", err, body)
			}
			for k := range keys {
				seen[k] = true
				if !expected[k] {
					t.Errorf("unexpected JSON field %q surfaced — update the documented set in this test AND web/src/api/client.ts so the frontend type doesn't drift\nfull body: %s", k, body)
				}
			}
		}
	}

	// Run 1: windows path → exercises windows_note + (when probe is
	// skipped because platform_supported=false) probe_skipped_note.
	collect(computeLocalProviderStatuses(context.Background(), ov))

	// Run 2: darwin happy path → exercises install/start/probed/etc.
	// goarch=arm64 because mlx-lm's platformAllowed gate now requires
	// Apple Silicon — without this the run would mark mlx-lm
	// PlatformSupported=false and skip the install/start payload, so
	// `install` / `start` wouldn't show up in `seen` and the field-set
	// assertion below would fail.
	ov.goos = "darwin"
	ov.goarch = "arm64"
	collect(computeLocalProviderStatuses(context.Background(), ov))

	// Run 3: linux non-loopback ollama → exercises probe_skipped_note
	// via the security guardrail (loopback-only). mlx-lm and exo keep
	// loopback defaults so their probes still fire; the security
	// invariant is "no probe to the *non-loopback* URL", not "no
	// probe at all this run".
	ov.goos = "linux"
	t.Setenv("WUPHF_OLLAMA_BASE_URL", "http://10.0.0.99:11434/v1")
	ov.probe = func(_ context.Context, _ string, baseURL string) (bool, string, bool) {
		if strings.Contains(baseURL, "10.0.0.99") {
			t.Errorf("probe should not fire for non-loopback URL %q", baseURL)
		}
		return true, "loaded-x", true
	}
	collect(computeLocalProviderStatuses(context.Background(), ov))

	for k := range expected {
		if !seen[k] {
			t.Errorf("documented field %q never surfaced across all goos paths — either the field was dropped or a path stopped populating it", k)
		}
	}
}
