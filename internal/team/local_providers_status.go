package team

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/nex-crm/wuphf/internal/config"
	"github.com/nex-crm/wuphf/internal/provider"
	"github.com/nex-crm/wuphf/internal/runtimebin"
)

// LocalProviderStatus is the doctor-panel payload for one local OpenAI-
// compatible runtime. The Settings UI renders one card per kind: green
// (Reachable=true), yellow (BinaryInstalled=true but Reachable=false —
// installed but not started), or red (BinaryInstalled=false). The
// Install field carries copy-paste shell snippets the user runs
// themselves; the broker never executes them.
type LocalProviderStatus struct {
	Kind              string            `json:"kind"`
	BinaryInstalled   bool              `json:"binary_installed"`
	BinaryPath        string            `json:"binary_path,omitempty"`
	BinaryVersion     string            `json:"binary_version,omitempty"`
	Endpoint          string            `json:"endpoint"`
	Model             string            `json:"model"`
	Reachable         bool              `json:"reachable"`
	LoadedModel       string            `json:"loaded_model,omitempty"`
	Probed            bool              `json:"probed"`
	ProbeSkippedNote  string            `json:"probe_skipped_note,omitempty"`
	PlatformSupported bool              `json:"platform_supported"`
	WindowsNote       string            `json:"windows_note,omitempty"`
	Install           map[string]string `json:"install,omitempty"`
	Start             map[string]string `json:"start,omitempty"`
	Notes             []string          `json:"notes,omitempty"`
}

// localProviderSpec describes the per-kind detection inputs.
type localProviderSpec struct {
	kind            string
	binaryName      string
	versionArgs     []string
	platformAllowed func(goos, goarch string) bool
	install         map[string]string
	start           map[string]string
	notes           []string
}

var localProviderSpecs = []localProviderSpec{
	{
		kind:       provider.KindAtlasCloud,
		binaryName: "",
		platformAllowed: func(_, _ string) bool {
			return true
		},
		notes: []string{
			"Set ATLASCLOUD_API_KEY or WUPHF_ATLASCLOUD_API_KEY before starting WUPHF. Remote Atlas Cloud endpoints are not probed from the broker; status reflects whether an API key is configured.",
		},
	},
	{
		kind:        provider.KindMLXLM,
		binaryName:  "mlx_lm.server",
		versionArgs: []string{"--version"},
		platformAllowed: func(goos, goarch string) bool {
			// MLX requires Apple Silicon — checking only goos=="darwin"
			// would mark Intel Macs as supported, where MLX wheels
			// don't load. The wizard tile + Settings doctor card both
			// key off PlatformSupported, so getting this wrong sends
			// Intel-Mac users into a "Running" UI for a runtime that
			// can't start.
			return goos == "darwin" && goarch == "arm64"
		},
		install: map[string]string{
			"macos": "pipx install mlx-lm",
		},
		start: map[string]string{
			"macos": "mlx_lm.server --model mlx-community/Qwen2.5-Coder-7B-Instruct-4bit --host 127.0.0.1 --port 8080",
		},
		notes: []string{
			"Requires Apple Silicon. The 7B model is the safe default for first-run; bump to 32B on a 64GB+ Mac via the Model field above.",
		},
	},
	{
		kind:        provider.KindOllama,
		binaryName:  "ollama",
		versionArgs: []string{"--version"},
		platformAllowed: func(goos, _ string) bool {
			// Ollama runs on macOS / Linux natively (both arm64 and
			// amd64). On Windows we recommend WSL2 rather than the
			// native installer to keep the supported surface narrow.
			return goos == "darwin" || goos == "linux"
		},
		install: map[string]string{
			"macos": "brew install ollama && brew services start ollama",
			"linux": "curl -fsSL https://ollama.com/install.sh | sh",
		},
		start: map[string]string{
			"macos": "ollama pull qwen2.5-coder:7b-instruct-q4_K_M",
			"linux": "ollama pull qwen2.5-coder:7b-instruct-q4_K_M",
		},
	},
	{
		kind:        provider.KindExo,
		binaryName:  "exo",
		versionArgs: []string{"--version"},
		platformAllowed: func(goos, _ string) bool {
			return goos == "darwin" || goos == "linux"
		},
		install: map[string]string{
			"macos": "pipx install exo",
			"linux": "pipx install exo",
		},
		start: map[string]string{
			"macos": "exo",
			"linux": "exo",
		},
		notes: []string{
			"Exo distributes inference across multiple machines. On a single Mac it offers little over MLX-LM directly; install on a second machine and run `exo` on each to enable.",
		},
	},
	{
		kind:        provider.KindHermesAgent,
		binaryName:  "hermes",
		versionArgs: []string{"--version"},
		platformAllowed: func(goos, _ string) bool {
			return goos == "darwin" || goos == "linux"
		},
		install: map[string]string{
			"macos": "curl -fsSL https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.sh | bash",
			"linux": "curl -fsSL https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.sh | bash",
		},
		start: map[string]string{
			"macos": "hermes gateway",
			"linux": "hermes gateway",
		},
		notes: []string{
			"Hermes Agent is reached through its OpenAI-compatible API server on port 8642. If you set API_SERVER_KEY for Hermes, set the same value as WUPHF_HERMES_AGENT_API_KEY before starting WUPHF.",
		},
	},
	{
		kind:        provider.KindOpenclawHTTP,
		binaryName:  "openclaw",
		versionArgs: []string{"--version"},
		platformAllowed: func(goos, _ string) bool {
			return goos == "darwin" || goos == "linux"
		},
		install: map[string]string{
			"macos": "curl -fsSL https://openclaw.ai/install.sh | bash -s -- --no-onboard",
			"linux": "curl -fsSL https://openclaw.ai/install.sh | bash -s -- --no-onboard",
		},
		start: map[string]string{
			"macos": "openclaw gateway run",
			"linux": "openclaw gateway run",
		},
		notes: []string{
			"OpenClaw Gateway's OpenAI-compatible endpoint is disabled by default. Enable gateway.http.endpoints.chatCompletions.enabled, then use gateway.auth.token as WUPHF_OPENCLAW_HTTP_API_KEY, OPENCLAW_GATEWAY_TOKEN, or WUPHF_OPENCLAW_TOKEN.",
		},
	},
}

// localProvidersStatusOverrides lets tests stub binary detection,
// version probing, HTTP probing, and runtime.GOOS/GOARCH without
// exec'ing or pinning the test box's architecture.
type localProvidersStatusOverrides struct {
	lookPath func(name string) (string, error)
	runVer   func(ctx context.Context, path string, args []string) (string, error)
	probe    func(ctx context.Context, kind, baseURL string) (reachable bool, loadedModel string, ok bool)
	goos     string
	goarch   string
}

// defaultLocalProvidersOverrides returns the production wiring.
func defaultLocalProvidersOverrides() localProvidersStatusOverrides {
	return localProvidersStatusOverrides{
		lookPath: runtimebin.LookPath,
		runVer:   runVersionCommand,
		probe:    probeOpenAICompatEndpoint,
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
	}
}

// handleLocalProvidersStatus serves GET /status/local-providers. The
// response is the doctor-panel payload for the Settings UI: one entry
// per registered local OpenAI-compatible kind, with binary-installed
// and reachable flags plus copy-paste install commands.
func (b *Broker) handleLocalProvidersStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out := computeLocalProviderStatuses(r.Context(), defaultLocalProvidersOverrides())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// computeLocalProviderStatuses runs the detection probes and returns
// the response payload. Pulled out as its own function so tests can
// drive it with a stubbed overrides struct without spinning up a
// broker.
func computeLocalProviderStatuses(ctx context.Context, ov localProvidersStatusOverrides) []LocalProviderStatus {
	out := make([]LocalProviderStatus, 0, len(localProviderSpecs))
	for _, spec := range localProviderSpecs {
		out = append(out, computeOneStatus(ctx, spec, ov))
	}
	return out
}

func computeOneStatus(ctx context.Context, spec localProviderSpec, ov localProvidersStatusOverrides) LocalProviderStatus {
	st := LocalProviderStatus{
		Kind:              spec.kind,
		PlatformSupported: spec.platformAllowed(ov.goos, ov.goarch),
		Install:           spec.install,
		Start:             spec.start,
		Notes:             append([]string(nil), spec.notes...),
	}
	if ov.goos == "windows" {
		st.WindowsNote = "Local LLM runtimes don't have native Windows support today. Run wuphf inside WSL2 (Ubuntu) and install the runtime there — the broker will then detect it from inside WSL."
	}

	// Resolve the effective endpoint by layering env > config > the
	// kind's registered compile-time defaults. Without the third layer
	// the UI would show a blank URL until the user typed something.
	defBaseURL, defModel := provider.OpenAICompatDefaults(spec.kind)
	endpoint, model := config.ResolveProviderEndpoint(spec.kind, defBaseURL, defModel)
	st.Endpoint = endpoint
	st.Model = model

	if spec.binaryName == "" {
		if provider.OpenAICompatAPIKey(spec.kind) != "" {
			st.BinaryInstalled = true
			st.BinaryVersion = "API key configured"
		}
	} else if path, err := ov.lookPath(spec.binaryName); err == nil {
		st.BinaryInstalled = true
		st.BinaryPath = path
		// Best-effort version capture; failures are silent — we already
		// know the binary exists, the version is just nice-to-have.
		verCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if v, err := ov.runVer(verCtx, path, spec.versionArgs); err == nil {
			st.BinaryVersion = strings.TrimSpace(v)
		}
		cancel()
	}

	// Probe the endpoint for reachability — but only if it's loopback.
	// Probing arbitrary remote URLs from the broker would let a hostile
	// config trigger outbound traffic to anywhere, and a slow remote
	// can hang the Settings page render.
	switch {
	case !st.PlatformSupported:
		// WindowsNote (above) already explains the platform gate;
		// don't add a second note that misattributes the skip to a
		// non-loopback URL. Probed stays false; UI uses
		// PlatformSupported to render the disabled state.
	case isLoopbackBaseURL(endpoint):
		probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		reachable, loadedModel, _ := ov.probe(probeCtx, spec.kind, endpoint)
		cancel()
		st.Reachable = reachable
		st.LoadedModel = loadedModel
		st.Probed = true
	case spec.binaryName == "" && endpoint != "":
		st.Reachable = st.BinaryInstalled
		if st.BinaryInstalled {
			st.ProbeSkippedNote = "Remote endpoint is not probed; API key is configured."
		} else {
			st.ProbeSkippedNote = "Remote endpoint is not probed; set the API key before using this runtime."
		}
	case endpoint != "":
		st.Probed = false
		st.ProbeSkippedNote = "Reachability probe is loopback-only; configured endpoint is non-local."
	}

	return st
}

// isLoopbackBaseURL reports whether the configured base URL points at
// 127.0.0.1, ::1, or localhost. Any DNS-resolvable hostname is treated
// as non-loopback (we don't want to resolve in the probe path).
func isLoopbackBaseURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	// DNS resolution is intentionally not done here — we don't want
	// the broker resolving arbitrary hostnames during a UI poll, and
	// localhost can be remapped (`/etc/hosts`) anyway. Treat the
	// literal name (case-insensitively, matching browser behavior) as
	// loopback; treat `0.0.0.0` as loopback too because servers bound
	// there are reachable from 127.0.0.1 in practice.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if host == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// runVersionCommand runs `<binary> <args...>` with a bounded ctx and
// returns the first line of output as the reported version. Heavy
// formatting differences across runtimes (e.g. mlx_lm.server prints
// `0.31.3`, ollama prints `ollama version is 0.1.31`) are surfaced
// verbatim — the UI doesn't try to parse them.
func runVersionCommand(ctx context.Context, path string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return first, nil
}

// probeOpenAICompatEndpoint hits `<baseURL>/models` (the OpenAI list-
// models endpoint, which all local OpenAI-compatible runtimes support) and
// reports whether the server is reachable plus, if the response is the
// expected shape, the first model id it advertises. Any error or
// non-2xx → reachable=false.
func probeOpenAICompatEndpoint(ctx context.Context, kind, baseURL string) (bool, string, bool) {
	endpoint := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "", false
	}
	if apiKey := provider.OpenAICompatAPIKey(kind); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 750 * time.Millisecond}).DialContext,
		},
		Timeout: 1500 * time.Millisecond,
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", true
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", true
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && len(body.Data) > 0 {
		return true, body.Data[0].ID, true
	}
	// Reachable but unrecognised JSON shape — still counts as reachable.
	return true, "", true
}
