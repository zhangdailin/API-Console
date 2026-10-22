package qoder

import (
	"bytes"
	"context"
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

// The probe sends one minimal prompt per inference node through one account
// and reports where the gateway admitted it: served, queued (and in which
// queue), or refused for another reason such as the per-host daily request
// count. It is an operator diagnostic run by hand; it spends at most a few
// output tokens per request and never reports a credential.
//
// Probes on 2026-10-21 settled what the request itself does not change: the
// client identity decides the queue class (QoderWork p4, IDE/CLI p3) but not
// whether a request is admitted, and neither do aliyun_user_type, the COSY
// identity layout or the client version. What remains worth measuring is the
// node and the time of day, which is what these variants cover.

// ProbeResult is the gateway's answer to one probe request.
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
}

// probeVariant is one node a probe request is sent to.
type probeVariant struct {
	name string
	host string
}

// probeVariants is the current request on the primary node (A) and on each
// failover node (F for api1, J for api3).
func (c *Client) probeVariants() []probeVariant {
	variants := []probeVariant{{name: "A current request", host: strings.TrimRight(c.endpoints.inference, "/")}}
	letters := []string{"F", "J", "L"}
	for i, fallback := range c.endpoints.inferenceFallbacks {
		if i >= len(letters) {
			break
		}
		variants = append(variants, probeVariant{name: letters[i] + " current request on " + hostName(fallback), host: fallback})
	}
	return variants
}

func hostName(base string) string {
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

// ProbeRoundOptions selects a repeated comparison.
type ProbeRoundOptions struct {
	// Variants are variant letters ("A", "F"); empty means every variant.
	Variants []string
	// Rounds is how many times each selected variant is sent.
	Rounds int
	// Interval separates consecutive requests, so every request meets the
	// gateway after the same pause and none is favoured by its position.
	Interval time.Duration
	// AlignWindow sends every request just after the low-priority queues'
	// per-minute admission window opens (see queue_window.go), one per
	// minute, instead of every Interval.
	AlignWindow bool
	// Progress, when set, receives each result as it arrives.
	Progress func(round int, result ProbeResult)
}

// ProbeRounds sends the selected variants Rounds times, flipping their order
// every round: a gateway that admits the first request after a pause would
// otherwise favour whichever variant is sent first.
func (c *Client) ProbeRounds(ctx context.Context, requestedModel string, opts ProbeRoundOptions) ([]ProbeResult, error) {
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	variants, err := selectProbeVariants(c.probeVariants(), opts.Variants)
	if err != nil {
		return nil, err
	}
	maxTokens := 16
	req := upstream.UpstreamRequest{
		Model:     requestedModel,
		MaxTokens: &maxTokens,
		Messages:  []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "Reply with the single word OK."}}},
	}
	model, err := c.resolveModel(req)
	if err != nil {
		return nil, err
	}
	rounds := max(opts.Rounds, 1)
	results := make([]ProbeResult, 0, rounds*len(variants))
	first := true
	for round := 1; round <= rounds; round++ {
		order := slices.Clone(variants)
		if round%2 == 0 {
			slices.Reverse(order)
		}
		for _, variant := range order {
			switch {
			case opts.AlignWindow:
				if err := sleepCtx(ctx, untilProbeWindow(time.Now())); err != nil {
					return results, err
				}
			case !first && opts.Interval > 0:
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
			result := c.probeOnce(ctx, variant, req, model, creds, fields)
			results = append(results, result)
			if opts.Progress != nil {
				opts.Progress(round, result)
			}
		}
	}
	return results, nil
}

func selectProbeVariants(all []probeVariant, letters []string) ([]probeVariant, error) {
	if len(letters) == 0 {
		return all, nil
	}
	selected := make([]probeVariant, 0, len(letters))
	for _, letter := range letters {
		letter = strings.ToUpper(strings.TrimSpace(letter))
		index := slices.IndexFunc(all, func(variant probeVariant) bool { return strings.HasPrefix(variant.name, letter+" ") })
		if index < 0 {
			return nil, fmt.Errorf("unknown probe variant %q", letter)
		}
		selected = append(selected, all[index])
	}
	return selected, nil
}

func (c *Client) probeOnce(ctx context.Context, variant probeVariant, req upstream.UpstreamRequest, model modelEntry, creds Credentials, fields RuntimeFields) ProbeResult {
	result := ProbeResult{Variant: variant.name, Host: hostName(variant.host)}
	fail := func(err error) ProbeResult {
		result.Outcome, result.Detail = "error", err.Error()
		return result
	}
	requestID, setID, sessionID, err := newChatUUIDs(c.entropy)
	if err != nil {
		return fail(err)
	}
	encoded, err := buildChatBodyProfile(req, model, sessionID, requestID, setID, c.clientVersion, "", sceneBusinessProduct)
	if err != nil {
		return fail(err)
	}

	url := chatURL(variant.host)
	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return fail(err)
	}
	if err := c.applyAuthHeadersBytes(httpReq, creds, fields, requestID, model.Key, model.Source, encoded, signPath(url)); err != nil {
		return fail(err)
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

// probeWindowSend is the second of the minute an aligned probe request is
// sent at: just after the admission window opens.
const probeWindowSend = queueWindowOpen + time.Second

// untilProbeWindow is the wait until the next minute's probeWindowSend.
func untilProbeWindow(now time.Time) time.Duration {
	intoMinute := time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())
	if intoMinute < probeWindowSend {
		return probeWindowSend - intoMinute
	}
	return time.Minute - intoMinute + probeWindowSend
}
