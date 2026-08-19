package provider

// Atlas Cloud exposes an OpenAI-compatible /v1/chat/completions surface for
// hosted model routing. Users can override both values with
// WUPHF_ATLASCLOUD_BASE_URL / WUPHF_ATLASCLOUD_MODEL or
// config.ProviderEndpoints["atlascloud"].
const (
	defaultAtlasCloudBaseURL = "https://api.atlascloud.ai/v1"
	defaultAtlasCloudModel   = "qwen/qwen3.5-flash"
)

func init() {
	Register(&Entry{
		Kind:     KindAtlasCloud,
		StreamFn: NewOpenAICompatStreamFn(KindAtlasCloud, defaultAtlasCloudBaseURL, defaultAtlasCloudModel),
		Capabilities: Capabilities{
			PaneEligible:    false,
			SupportsOneShot: false,
		},
	})
}
