package llm

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StreamEventType enumerates the normalized streaming events every adapter
// emits, regardless of wire format.
type StreamEventType string

const (
	SEMessageStart      StreamEventType = "message_start"
	SETextDelta         StreamEventType = "text_delta"
	SEThinkingDelta     StreamEventType = "thinking_delta"
	SEThinkingSignature StreamEventType = "thinking_signature"
	SEToolUseStart      StreamEventType = "tool_use_start"
	SEToolInputJSON     StreamEventType = "tool_input_delta"
	// SEToolUseStop signals that a tool_use content block has finished streaming
	// its arguments (Anthropic content_block_stop, OpenAI Responses
	// response.function_call_arguments.done / response.output_item.done), allowing
	// the harness to dispatch the tool immediately without waiting for message_stop.
	SEToolUseStop StreamEventType = "tool_use_stop"
	// SEToolUseSignature carries Gemini's thought_signature for the current
	// tool_use block (see ContentBlock.ThoughtSignature). Mirrors
	// SEThinkingSignature's role for thinking blocks; emitted only by the
	// OpenAI adapter against Gemini's compatibility endpoint.
	SEToolUseSignature StreamEventType = "tool_use_signature"
	SEMessageDelta     StreamEventType = "message_delta" // carries stop_reason / usage
	SEMessageStop      StreamEventType = "message_stop"
	// SERedactedThinking carries a whole redacted_thinking block; Text is its
	// opaque data. Emitted only by the Anthropic adapter.
	SERedactedThinking StreamEventType = "redacted_thinking"
)

// StreamEvent is one normalized model-level streaming event (FR-03.5).
type StreamEvent struct {
	Type       StreamEventType
	Text       string // text/thinking/tool_input JSON fragment
	ToolID     string // tool_use_start
	ToolName   string // tool_use_start
	StopReason string // message_delta
	// ToolIndex is the provider-assigned tool_calls[] index, carried on
	// SEToolUseStart and SEToolUseSignature so a signature can be matched to the
	// call it belongs to rather than to whichever call started last. Only the
	// OpenAI Chat Completions adapter populates it (that wire format indexes
	// parallel calls); elsewhere it is 0 and consumers fall back to the most
	// recently started call.
	ToolIndex int
	// Usage arrives on message_start and message_delta. For FormatAnthropic the
	// two ECHO the input side (input_tokens, cache read/creation) rather than
	// partitioning it, so folding them with Usage.Add double-counts those
	// fields. Use Usage.MergeEcho, which maxes the echoed input fields and
	// accumulates output — see Accumulator.Add.
	Usage Usage
}

// Format selects the wire protocol of a Provider.
type Format string

const (
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
	// FormatOpenAIResponses speaks the OpenAI Responses API (POST /v1/responses),
	// distinct from FormatOpenAI's Chat Completions (/chat/completions).
	FormatOpenAIResponses Format = "openai-responses"
	// FormatDevin speaks Cognition/Devin's Cascade Connect+Protobuf API.
	FormatDevin Format = "devin"
	// FormatAntigravity delegates authenticated inference to Google's `agy acp` subprocess.
	FormatAntigravity Format = "antigravity-cli"
)

// MaxTokensFieldCompletion is the Config.MaxTokensField value that moves the
// OpenAI Chat Completions output cap onto "max_completion_tokens". Any other
// value keeps the classic "max_tokens".
const MaxTokensFieldCompletion = "max_completion_tokens"

// Config configures the built-in providers (FR-03.4).
type Config struct {
	Format     Format
	BaseURL    string
	APIKey     string
	Model      string
	APIVersion string // Anthropic only; default 2023-06-01
	HTTPClient *http.Client
	// Proxy, when set, routes all model requests through the given proxy URL —
	// "http://", "https://" or "socks5://" (optionally with user:pass@). It only
	// affects the built-in client: when empty, the built-in client falls back to
	// the standard HTTP_PROXY / HTTPS_PROXY / NO_PROXY environment variables.
	// Ignored when the caller supplies its own HTTPClient.
	Proxy string
	// MaxRetries bounds transient-failure retries on the model request
	// establishment (connection reset / timeout / 429 / 5xx). 0 = default 3;
	// negative disables retrying. Only the pre-stream phase is retried (safe);
	// a mid-stream drop is surfaced as an error.
	MaxRetries int
	// RetryInterval, when > 0, replaces the exponential backoff between
	// establishment retries with this FIXED wait. 0 (default) keeps the
	// 0.5s/1s/2s… ladder capped at 8s. A host that wants a predictable,
	// operator-set cadence sets both MaxRetries and this.
	RetryInterval time.Duration
	// EmptyResponseRetries bounds how many times a completed-but-EMPTY response
	// is re-requested (FormatOpenAI only; see emptyResponseRetries). 0 = default
	// 2; negative disables. Separate from MaxRetries because an empty-response
	// retry re-sends the whole prompt rather than re-establishing a request.
	EmptyResponseRetries int
	// EmptyResponseInterval, when > 0, replaces the exponential backoff before
	// each empty-response retry with this FIXED wait. 0 keeps the ladder.
	EmptyResponseInterval time.Duration
	// RateLimit, when set, bounds the request rate shared across every caller of
	// the built Provider (per-second and/or per-minute). Retries do not count.
	RateLimit *RateLimit

	// ThinkingType toggles extended/reasoning mode via the request's
	// "thinking":{"type":...} field, for both formats. "" omits it (provider
	// default); "enabled"/"disabled" are sent verbatim.
	ThinkingType string
	// ReasoningEffort sets the reasoning strength. "" omits it. For OpenAI it maps
	// to "reasoning_effort"; for Anthropic to "output_config":{"effort":...}.
	// Typical values: low / medium / high / max.
	ReasoningEffort string
	// MaxTokensField selects which request key carries the output cap on
	// FormatOpenAI (Chat Completions). "" (default) sends the classic
	// "max_tokens"; MaxTokensFieldCompletion sends "max_completion_tokens"
	// instead. OpenAI's reasoning models reject "max_tokens" outright
	// (unsupported_parameter) and accept only the newer key, whose budget covers
	// reasoning tokens plus visible output; most OpenAI-compatible gateways still
	// take the old one, hence the opt-in default. Exactly one key is ever sent.
	// Ignored by the Anthropic and Responses formats, which name the field
	// themselves ("max_tokens" / "max_output_tokens").
	MaxTokensField string

	// PromptCacheEnabled controls automatic prompt cache breakpoints (Anthropic)
	// and prompt_cache_key emission (OpenAI/Responses). nil or true = enabled;
	// explicit false disables all cache annotations.
	PromptCacheEnabled *bool
	// PromptCacheTTL optionally sets Anthropic's cache TTL (e.g. "1h"). Empty = default (5m).
	PromptCacheTTL string
	// PromptCacheRetention optionally sets OpenAI's prompt_cache_retention (e.g. "24h").
	PromptCacheRetention string
	// OpenAICacheControl, when true, emits Anthropic-style cache_control blocks on
	// OpenAI Chat Completions requests (for gateways like OpenRouter routing to Claude).
	OpenAICacheControl bool
	// StripOldReasoning, when true, omits reasoning_content on assistant turns
	// older than the latest user turn in OpenAI Chat Completions requests.
	StripOldReasoning bool
	// StreamIdleTimeout bounds how long an open SSE stream may go without
	// delivering any data frame before timing out and allowing failover.
	// 0 uses the default (45s, or NORMA_STREAM_IDLE_TIMEOUT_SEC); negative disables.
	StreamIdleTimeout time.Duration
	// TokenFunc, when set, dynamically resolves an OAuth Bearer access token
	// before each request (and is re-invoked with forceRefresh=true once on 401).
	TokenFunc func(ctx context.Context, forceRefresh bool) (string, error)
	// AgyCredFunc, when set, dynamically resolves a full typed AgyCredential for
	// FormatAntigravity without packing JSON into TokenFunc.
	AgyCredFunc func(ctx context.Context, forceRefresh bool) (AgyCredential, error)
	// UseBearerAuth sends `Authorization: Bearer <key>` instead of `x-api-key`
	// on Anthropic Messages requests (used by Claude OAuth and OpenCode gateways).
	UseBearerAuth bool
	// ExtraHeaders are applied to every outbound LLM request (e.g. anthropic-beta).
	ExtraHeaders map[string]string
	// ToolsAsInputItem sends Responses API tools as an `additional_tools` input
	// item instead of top-level `tools` (required by Sign in with ChatGPT).
	ToolsAsInputItem bool

	limiter *rateLimiter // built from RateLimit by NewProvider; shared via the provider
}

type maxRetriesCtxKey struct{}

// WithMaxRetries overrides Config.MaxRetries for a single call context (used by
// llmpool to fail over immediately when fallback profiles exist).
func WithMaxRetries(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxRetriesCtxKey{}, n)
}

func maxRetriesFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	v, ok := ctx.Value(maxRetriesCtxKey{}).(int)
	return v, ok
}

func (c Config) streamIdleTimeout() time.Duration {
	if c.StreamIdleTimeout < 0 {
		return 0
	}
	if c.StreamIdleTimeout > 0 {
		return c.StreamIdleTimeout
	}
	if raw := strings.TrimSpace(os.Getenv("NORMA_STREAM_IDLE_TIMEOUT_SEC")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil {
			if sec <= 0 {
				return 0
			}
			return time.Duration(sec) * time.Second
		}
	}
	return 45 * time.Second
}

func (c Config) promptCacheEnabled() bool {
	return c.PromptCacheEnabled == nil || *c.PromptCacheEnabled
}

// retries returns the effective retry count (0 → default 3; negative → 0).
func (c Config) retries() int {
	if c.MaxRetries == 0 {
		return 3
	}
	if c.MaxRetries < 0 {
		return 0
	}
	return c.MaxRetries
}

// retryDelay is the wait before establishment retry number attempt (0-based):
// the configured fixed interval when set, the exponential ladder otherwise.
func (c Config) retryDelay(attempt int) time.Duration {
	if c.RetryInterval > 0 {
		return c.RetryInterval
	}
	return expBackoff(attempt)
}

// emptyRetries returns the effective empty-response retry count
// (0 → default emptyResponseRetries; negative → 0).
func (c Config) emptyRetries() int {
	if c.EmptyResponseRetries == 0 {
		return emptyResponseRetries
	}
	if c.EmptyResponseRetries < 0 {
		return 0
	}
	return c.EmptyResponseRetries
}

// emptyRetryDelay is the wait before empty-response retry number attempt.
func (c Config) emptyRetryDelay(attempt int) time.Duration {
	if c.EmptyResponseInterval > 0 {
		return c.EmptyResponseInterval
	}
	return expBackoff(attempt)
}

// Provider produces a completion, either streamed (Stream) or in a single
// non-streaming round-trip (Complete). Hosts may implement this interface to
// plug in private RPC backends (FR-03.7). Stream's iterator yields normalized
// events; a terminal error is delivered as (zero, err) and ends iteration.
// Complete performs a real non-streaming request (the wire carries
// stream:false and the whole JSON body is parsed at once) and returns the fully
// assembled assistant message, the provider stop reason, and cumulative usage.
type Provider interface {
	Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error]
	Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error)
}

// NewProvider builds a built-in provider for cfg.Format. Missing APIKey/BaseURL
// fall back to standard environment variables and public endpoints.
func NewProvider(cfg Config) (Provider, error) {
	if cfg.HTTPClient == nil {
		client, err := defaultHTTPClient(cfg.Proxy)
		if err != nil {
			return nil, err
		}
		cfg.HTTPClient = client
	}
	if cfg.RateLimit != nil {
		cfg.limiter = newRateLimiter(*cfg.RateLimit) // shared by the provider (cfg copies the pointer)
	}
	switch cfg.Format {
	case FormatAnthropic:
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
		}
		if cfg.APIVersion == "" {
			cfg.APIVersion = "2023-06-01"
		}
		return &anthropicProvider{cfg: cfg}, nil
	case FormatOpenAI:
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
		}
		return &openaiProvider{cfg: cfg}, nil
	case FormatOpenAIResponses:
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
		}
		return &openaiResponsesProvider{cfg: cfg}, nil
	case FormatDevin:
		if cfg.BaseURL == "" {
			cfg.BaseURL = DevinDefaultBaseURL
		}
		return newDevinProvider(cfg), nil
	case FormatAntigravity:
		return newAntigravityProvider(cfg), nil
	default:
		return nil, fmt.Errorf("llm: unknown format %q (use anthropic, openai, openai-responses, devin or antigravity-cli)", cfg.Format)
	}
}

var sharedDefaultTransports sync.Map // proxy string -> *http.Transport

// defaultHTTPClient builds the built-in HTTP client backed by a shared
// proxy-keyed HTTP/2 transport pool so multiple providers to the same host
// reuse warm TCP/TLS connections.
func defaultHTTPClient(proxy string) (*http.Client, error) {
	proxy = strings.TrimSpace(proxy)
	if v, ok := sharedDefaultTransports.Load(proxy); ok {
		return &http.Client{Transport: v.(*http.Transport)}, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 64
	tr.IdleConnTimeout = 90 * time.Second
	tr.TLSHandshakeTimeout = 5 * time.Second
	tr.ReadBufferSize = 64 << 10
	tr.WriteBufferSize = 64 << 10
	tr.ForceAttemptHTTP2 = true
	tr.ResponseHeaderTimeout = responseHeaderTimeoutFromEnv()
	if proxy == "" {
		tr.Proxy = http.ProxyFromEnvironment
		actual, _ := sharedDefaultTransports.LoadOrStore(proxy, tr)
		return &http.Client{Transport: actual.(*http.Transport)}, nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5": // supported by net/http's Transport.Proxy
	case "":
		return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
	default:
		return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", u.Scheme)
	}
	tr.Proxy = http.ProxyURL(u)
	actual, _ := sharedDefaultTransports.LoadOrStore(proxy, tr)
	return &http.Client{Transport: actual.(*http.Transport)}, nil
}

func responseHeaderTimeoutFromEnv() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("NORMA_RESPONSE_HEADER_TIMEOUT_SEC")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil {
			if sec <= 0 {
				return 0
			}
			return time.Duration(sec) * time.Second
		}
	}
	return 60 * time.Second
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// joinSystem renders the system segments into a single string.
func joinSystem(segs []string) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}
