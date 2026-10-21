package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// The identity probe answers one question the gateway's refusals cannot: which
// part of a request decides the queue it is admitted to. Every variant sends
// the same minimal prompt with the same credential; only the client identity
// changes (body fields, headers, the version signed into the COSY payload, and
// the host). Comparing the queue each variant lands in shows which identity the
// gateway still serves.
//
// It is an operator diagnostic, run by hand against one account. It spends at
// most a few output tokens per variant and never reports a credential.

// ProbeResult is the gateway's answer to one identity variant.
type ProbeResult struct {
	Variant string
	Host    string
	// Outcome is "served" when the stream produced model output, "queued" for a
	// 10605 queue refusal, and "error" otherwise.
	Outcome   string
	Status    int
	QueueType string
	// Available is the refusal's serviceAvailable flag; empty when absent.
	Available string
	SubCode   string
	RetryWait string
	Detail    string
	Elapsed   time.Duration
	// Search mode reads the whole answer: its text, the tools the model
	// called, and the search-related markers found in the raw stream.
	Answer        string
	ToolCalls     []string
	SearchMarkers []string
}

// probeIdentity is one client identity the probe presents.
type probeIdentity struct {
	name    string
	host    string
	version string
	body    func(map[string]any)
	headers func(http.Header)
	// referenceInfo signs with the reference COSY identity layout (which
	// carries user_type); reference sends the whole reference request.
	referenceInfo bool
	reference     bool
	// fullRead reads the whole answer instead of stopping at its first token.
	fullRead bool
}

// probeIdentities lists the identities compared, the current one first.
func (c *Client) probeIdentities(userType string) []probeIdentity {
	primary := strings.TrimRight(c.endpoints.inference, "/")
	withUserType := func(body map[string]any) { body["aliyun_user_type"] = userType }
	// The pre-capture IDE identity this channel used before 2026-09-29.
	ide := func(body map[string]any) {
		body["session_type"] = "qoder"
		if business, ok := body["business"].(map[string]any); ok {
			business["product"] = "ide"
			business["version"] = "1.0.10"
		}
	}
	ideHeaders := func(h http.Header) {
		h.Set("Cosy-Clienttype", "5")
		h.Set("Cosy-Business-Product", "ide")
		h.Set("Cosy-Scene", "assistant")
		h.Set("Cosy-Version", "1.0.10")
	}
	// The Qoder CLI identity the reference bridge presents.
	cli := func(body map[string]any) {
		body["session_type"] = "qodercli"
		body["aliyun_user_type"] = userType
		delete(body, "is_reply")
		delete(body, "is_retry")
		delete(body, "system")
		if business, ok := body["business"].(map[string]any); ok {
			for _, key := range []string{"product", "stage", "sub_task", "type", "version"} {
				delete(business, key)
			}
		}
	}
	cliHeaders := func(h http.Header) {
		for _, key := range []string{"Cosy-Scene", "Cosy-Business-Product", "Cosy-Business-Type", "Cosy-Machineos", "Sec-Fetch-Mode", "Accept-Language", "Traceparent", "Cache-Control"} {
			h.Del(key)
		}
		h.Set("Cosy-Clienttype", "5")
		h.Set("Cosy-Version", "0.1.43")
		h.Set("Cosy-Data-Policy", "AGREE")
		h.Set("User-Agent", "Go-http-client/2.0")
		h.Set("Accept-Encoding", "identity")
	}
	both := func(body, extra func(map[string]any)) func(map[string]any) {
		return func(m map[string]any) { body(m); extra(m) }
	}
	identities := []probeIdentity{
		{name: "A current (qoder_work)", host: primary, version: c.clientVersion},
		{name: "B current + aliyun_user_type", host: primary, version: c.clientVersion, body: withUserType},
		{name: "C ide identity", host: primary, version: "1.0.10", body: ide, headers: ideHeaders},
		{name: "D ide + aliyun_user_type", host: primary, version: "1.0.10", body: both(ide, withUserType), headers: ideHeaders},
		{name: "E cli identity", host: primary, version: "0.1.43", body: cli, headers: cliHeaders},
		{name: "H current + reference info", host: primary, version: c.clientVersion, referenceInfo: true},
		{name: "I full reference request", host: primary, version: referenceCosyVersion, reference: true},
	}
	// Every failover node is probed: the daily request count is kept per
	// host, so whether a node also has its own admission capacity is the
	// question these variants answer. api1 gets F/G and api3 J/K.
	letters := [][2]string{{"F", "G"}, {"J", "K"}, {"L", "M"}}
	for i, fallback := range c.endpoints.inferenceFallbacks {
		if i >= len(letters) {
			break
		}
		identities = append(identities,
			probeIdentity{name: letters[i][0] + " current on " + hostName(fallback), host: fallback, version: c.clientVersion},
			probeIdentity{name: letters[i][1] + " cli identity on " + hostName(fallback), host: fallback, version: "0.1.43", body: cli, headers: cliHeaders},
		)
	}
	return identities
}

func hostName(base string) string {
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

// ProbeIdentities sends the probe prompt once per identity and reports where
// the gateway admitted each one.
func (c *Client) ProbeIdentities(ctx context.Context, requestedModel string) ([]ProbeResult, error) {
	return c.ProbeRounds(ctx, requestedModel, ProbeRoundOptions{Rounds: 1})
}

// ProbeRoundOptions selects a repeated comparison.
type ProbeRoundOptions struct {
	// Variants are identity letters ("A", "E"); empty means every identity.
	Variants []string
	// Rounds is how many times each selected identity is sent.
	Rounds int
	// Interval separates consecutive requests, so every request meets the
	// gateway after the same pause and none is favoured by its position.
	Interval time.Duration
	// Progress, when set, receives each result as it arrives.
	Progress func(round int, result ProbeResult)
	// Search replaces the identity variants with the web-search variants
	// (see searchIdentities) and asks a question only a search can answer.
	Search bool
}

// ProbeRounds sends the selected identities Rounds times. The order flips
// every round: a single pass always sent the current identity first, and the
// only request served in four passes was that first one, which a gateway that
// admits one request per window favours regardless of identity.
func (c *Client) ProbeRounds(ctx context.Context, requestedModel string, opts ProbeRoundOptions) ([]ProbeResult, error) {
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	all := c.probeIdentities(c.aliyunUserType())
	question := "Reply with the single word OK."
	maxTokens := 16
	if opts.Search {
		all = c.searchIdentities()
		question = searchProbeQuestion
		maxTokens = 600
	}
	identities, err := selectProbeIdentities(all, opts.Variants)
	if err != nil {
		return nil, err
	}
	req := upstream.UpstreamRequest{
		Model:     requestedModel,
		MaxTokens: &maxTokens,
		Messages:  []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: question}}},
	}
	model, err := c.resolveModel(req)
	if err != nil {
		return nil, err
	}
	rounds := max(opts.Rounds, 1)
	results := make([]ProbeResult, 0, rounds*len(identities))
	first := true
	for round := 1; round <= rounds; round++ {
		order := slices.Clone(identities)
		if round%2 == 0 {
			slices.Reverse(order)
		}
		for _, identity := range order {
			if !first && opts.Interval > 0 {
				if err := sleepCtx(ctx, opts.Interval); err != nil {
					return results, err
				}
			}
			first = false
			// The credential is re-read per request: a long comparison can
			// outlive one access token.
			creds, err := c.ensureAccessToken(ctx)
			if err != nil {
				return results, fmt.Errorf("prepare credential: %w", err)
			}
			fields, err := c.ensureRuntimeFields(ctx, creds)
			if err != nil {
				return results, fmt.Errorf("prepare runtime fields: %w", err)
			}
			result := c.probeOnce(ctx, identity, req, model, creds, fields)
			results = append(results, result)
			if opts.Progress != nil {
				opts.Progress(round, result)
			}
		}
	}
	return results, nil
}

func selectProbeIdentities(all []probeIdentity, letters []string) ([]probeIdentity, error) {
	if len(letters) == 0 {
		return all, nil
	}
	selected := make([]probeIdentity, 0, len(letters))
	for _, letter := range letters {
		letter = strings.ToUpper(strings.TrimSpace(letter))
		index := slices.IndexFunc(all, func(identity probeIdentity) bool { return strings.HasPrefix(identity.name, letter+" ") })
		if index < 0 {
			return nil, fmt.Errorf("unknown probe variant %q", letter)
		}
		selected = append(selected, all[index])
	}
	return selected, nil
}

func (c *Client) probeOnce(ctx context.Context, identity probeIdentity, req upstream.UpstreamRequest, model modelEntry, creds Credentials, fields RuntimeFields) ProbeResult {
	result := ProbeResult{Variant: identity.name, Host: hostName(identity.host)}
	fail := func(err error) ProbeResult {
		result.Outcome, result.Detail = "error", err.Error()
		return result
	}
	if identity.referenceInfo || identity.reference {
		reference, err := c.referenceRuntimeFields(creds)
		if err != nil {
			return fail(err)
		}
		fields = reference
	}
	requestID, setID, sessionID, err := newChatUUIDs(c.entropy)
	if err != nil {
		return fail(err)
	}
	encoded, err := buildChatBodyProfile(req, model, sessionID, requestID, setID, c.clientVersion, "", sceneBusinessProduct)
	if err != nil {
		return fail(err)
	}
	if identity.reference {
		raw, err := decodeBody(encoded)
		if err != nil {
			return fail(err)
		}
		if raw, err = referenceBody(raw, c.aliyunUserType()); err != nil {
			return fail(err)
		}
		encoded = EncodeBody(raw)
	}
	if identity.body != nil {
		raw, err := decodeBody(encoded)
		if err != nil {
			return fail(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return fail(err)
		}
		identity.body(body)
		raw, err = json.Marshal(body)
		if err != nil {
			return fail(err)
		}
		encoded = EncodeBody(raw)
	}

	url := chatURL(identity.host)
	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return fail(err)
	}
	if identity.reference {
		if err := c.applyReferenceHeaders(httpReq, creds, fields, model.Key, model.Source, encoded, signPath(url)); err != nil {
			return fail(err)
		}
	} else if err := c.applyAuthHeadersBytes(httpReq, creds, fields, requestID, model.Key, model.Source, encoded, signPath(url)); err != nil {
		return fail(err)
	}
	if identity.headers != nil {
		identity.headers(httpReq.Header)
	}
	if !identity.reference && identity.version != c.clientVersion {
		// The client version is signed into the COSY payload, so a variant that
		// presents another version has to be signed with it.
		authorization, err := buildCOSYAuthorization(requestID, fields.EncryptUserInfo, identity.version, fields.Key, httpReq.Header.Get("Cosy-Date"), encoded, signPath(url))
		if err != nil {
			return fail(err)
		}
		httpReq.Header.Set("Authorization", authorization)
	}

	started := time.Now()
	resp, err := c.stream.Do(httpReq)
	result.Elapsed = time.Since(started)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = resp.Body.Close() }()
	result.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return describeProbeRefusal(result, classifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), raw), raw)
	}
	if identity.fullRead {
		return readSearchProbe(result, resp.Body, started)
	}
	// Stop at the first model output: the probe only needs to know it was served.
	served := false
	_, streamErr := consumeStreamObserved(resp.Body, func(m upstream.SSEMessage) {
		if !served && (m.Type == "model.text-delta" || m.Type == "model.reasoning-delta" || m.Type == "model.tool-call") {
			served = true
			cancel()
		}
	}, nil)
	result.Elapsed = time.Since(started)
	if served {
		result.Outcome = "served"
		return result
	}
	if streamErr == nil {
		streamErr = errors.New("stream finished without output")
	}
	return describeProbeRefusal(result, streamErr, []byte(streamErr.Error()))
}

// describeProbeRefusal fills the queue fields a 10605 refusal names.
func describeProbeRefusal(result ProbeResult, err error, raw []byte) ProbeResult {
	result.Outcome, result.Detail = "error", util.Truncate(strings.Join(strings.Fields(err.Error()), " "), 240)
	if errors.Is(err, ErrBusy) {
		result.Outcome = "queued"
	}
	text := strings.ReplaceAll(string(raw)+" "+err.Error(), `\`, "")
	result.QueueType = probeField(text, "queueType")
	result.Available = probeField(text, "serviceAvailable")
	result.SubCode = probeField(text, "subCode")
	result.RetryWait = probeField(text, "retryAfterSeconds")
	return result
}

// probeField reads `"key":value` out of a refusal however deeply it was quoted.
func probeField(text, key string) string {
	marker := `"` + key + `":`
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	value := strings.TrimLeft(text[index+len(marker):], ` "`)
	end := strings.IndexAny(value, `",}`)
	if end < 0 {
		return value
	}
	return value[:end]
}

// searchProbeQuestion can only be answered correctly with a live search, and
// asks for the source so a searched answer is recognisable.
const searchProbeQuestion = "Search the web: what is the most recent stable release of the Go programming language, and on what date was it released? Answer in one sentence and give the URL you found it at."

// searchIdentities declares a web search in the ways a client or a model
// platform might, each passed to the gateway unchanged, so the probe shows
// whether the gateway runs any of them. S0 declares none and is the baseline.
func (c *Client) searchIdentities() []probeIdentity {
	primary := strings.TrimRight(c.endpoints.inference, "/")
	withTools := func(tools ...any) func(map[string]any) {
		return func(body map[string]any) { body["tools"] = tools }
	}
	enableSearch := func(body map[string]any) {
		parameters, _ := body["parameters"].(map[string]any)
		if parameters == nil {
			parameters = map[string]any{}
		}
		parameters["enable_search"] = true
		body["parameters"] = parameters
	}
	return []probeIdentity{
		{name: "S0 no search tool", host: primary, version: c.clientVersion, fullRead: true},
		{name: "S1 anthropic web_search_20250305 tool", host: primary, version: c.clientVersion, fullRead: true,
			body: withTools(map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5})},
		{name: "S2 openai web_search tool", host: primary, version: c.clientVersion, fullRead: true,
			body: withTools(map[string]any{"type": "web_search"})},
		{name: "S3 parameters.enable_search", host: primary, version: c.clientVersion, fullRead: true, body: enableSearch},
	}
}

// searchMarkers are raw-stream words that show a search took place.
var searchMarkers = []string{"search", "citation", "reference", "url_citation", "\"url\""}

// readSearchProbe reads a whole answer, keeping a bounded copy of the raw
// stream to look for search evidence the parsed events would not carry.
func readSearchProbe(result ProbeResult, body io.Reader, started time.Time) ProbeResult {
	var raw bytes.Buffer
	tee := io.TeeReader(io.LimitReader(body, 1<<20), &limitedWriter{buf: &raw, limit: 256 << 10})
	var answer strings.Builder
	_, streamErr := consumeStreamObserved(tee, func(m upstream.SSEMessage) {
		switch m.Type {
		case "model.text-delta":
			if delta, ok := m.Event["delta"].(string); ok {
				answer.WriteString(delta)
			}
		case "model.tool-call":
			if name, ok := m.Event["toolName"].(string); ok {
				result.ToolCalls = append(result.ToolCalls, name)
			} else {
				result.ToolCalls = append(result.ToolCalls, fmt.Sprint(m.Event["name"]))
			}
		}
	}, nil, true)
	result.Elapsed = time.Since(started)
	result.Answer = util.Truncate(strings.Join(strings.Fields(answer.String()), " "), 400)
	lower := strings.ToLower(strings.ReplaceAll(raw.String(), `\`, ""))
	for _, marker := range searchMarkers {
		if count := strings.Count(lower, marker); count > 0 {
			result.SearchMarkers = append(result.SearchMarkers, fmt.Sprintf("%s=%d", strings.Trim(marker, `"`), count))
		}
	}
	if result.Answer != "" || len(result.ToolCalls) > 0 {
		result.Outcome = "served"
		return result
	}
	if streamErr == nil {
		streamErr = errors.New("stream finished without output")
	}
	return describeProbeRefusal(result, streamErr, []byte(streamErr.Error()))
}

// limitedWriter keeps the first limit bytes written to it and drops the rest.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}
