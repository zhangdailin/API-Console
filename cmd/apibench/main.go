// apibench measures real streaming Chat Completions through a gateway.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/util"
)

type result struct {
	AnswerCorrect    bool             `json:"answer_correct"`
	Index            int              `json:"index"`
	HTTPStatus       int              `json:"http_status"`
	RequestID        string           `json:"request_id,omitempty"`
	HeadersMS        float64          `json:"headers_ms"`
	E2EMS            float64          `json:"e2e_ms"`
	FirstGeneratedMS *float64         `json:"first_generated_ms,omitempty"`
	FirstVisibleMS   *float64         `json:"first_visible_ms,omitempty"`
	Phases           map[string]int64 `json:"http_phases,omitempty"`
	Finish           string           `json:"finish_reason,omitempty"`
	Done             bool             `json:"done"`
	VisibleChars     int              `json:"visible_bytes"`
	ReasoningChars   int              `json:"reasoning_bytes"`
	ToolOutput       bool             `json:"tool_output"`
	OutputTokens     int              `json:"output_tokens"`
	UsageReported    bool             `json:"usage_reported"`
	DeltaGapsMS      []float64        `json:"visible_delta_gaps_ms,omitempty"`
	Error            string           `json:"error,omitempty"`
}

func probe(client *http.Client, endpoint, key, model, effort string, maxTokens int, timeout time.Duration) result {
	started := time.Now()
	r := result{}
	elapsed := func() float64 { return float64(time.Since(started)) / float64(time.Millisecond) }
	body := map[string]interface{}{"model": model, "stream": true, "max_tokens": maxTokens, "stream_options": map[string]bool{"include_usage": true}, "messages": []map[string]string{{"role": "user", "content": "Output exactly the integers from 1 to 20 separated by spaces, with no explanation."}}}
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var phaseMu sync.Mutex
	var phaseSnapshot map[string]int64
	ctx = util.WithHTTPPhaseObserver(ctx, func(phases map[string]int64) { phaseMu.Lock(); phaseSnapshot = phases; phaseMu.Unlock() })
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(util.TraceHTTPPhases(req))
	phaseMu.Lock()
	r.Phases = phaseSnapshot
	phaseMu.Unlock()
	if err != nil {
		r.Error = "transport_error"
		r.E2EMS = elapsed()
		return r
	}
	defer resp.Body.Close()
	r.HeadersMS = elapsed()
	r.HTTPStatus = resp.StatusCode
	r.RequestID = resp.Header.Get("X-Orchids-Request-ID")
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		r.Error = "http_error"
		r.E2EMS = elapsed()
		return r
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		r.Error = "expected_sse"
		r.E2EMS = elapsed()
		return r
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var lastVisible float64
	var answer strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			r.Done = true
			break
		}
		if data == "" {
			continue
		}
		var p struct {
			Error   json.RawMessage
			Choices []struct {
				Finish string `json:"finish_reason"`
				Delta  struct {
					Content          string
					Reasoning        string
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				}
			}
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			}
		}
		if json.Unmarshal([]byte(data), &p) != nil {
			r.Error = "invalid_sse_json"
			break
		}
		if len(p.Error) > 0 && string(p.Error) != "null" {
			r.Error = "stream_error"
			break
		}
		for _, c := range p.Choices {
			now := elapsed()
			d := c.Delta
			if d.Content != "" || d.Reasoning != "" || d.ReasoningContent != "" || len(d.ToolCalls) > 0 {
				if r.FirstGeneratedMS == nil {
					t := now
					r.FirstGeneratedMS = &t
				}
			}
			if d.Content != "" {
				if r.FirstVisibleMS == nil {
					t := now
					r.FirstVisibleMS = &t
				}
				if lastVisible > 0 {
					r.DeltaGapsMS = append(r.DeltaGapsMS, now-lastVisible)
				}
				lastVisible = now
				r.VisibleChars += len(d.Content)
				if answer.Len() < 4096 {
					answer.WriteString(d.Content[:min(len(d.Content), 4096-answer.Len())])
				}
			}
			r.ReasoningChars += len(d.Reasoning) + len(d.ReasoningContent)
			r.ToolOutput = r.ToolOutput || len(d.ToolCalls) > 0
			if c.Finish != "" {
				r.Finish = c.Finish
			}
		}
		if p.Usage != nil {
			r.UsageReported = true
			r.OutputTokens = p.Usage.CompletionTokens
		}
	}
	if scanner.Err() != nil {
		r.Error = "stream_read_error"
	}
	if r.Error == "" && (!r.Done || r.Finish == "") {
		r.Error = "missing_terminal"
	}
	r.AnswerCorrect = strings.Join(strings.Fields(answer.String()), " ") == "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20"
	r.E2EMS = elapsed()
	return r
}

func distribution(values []float64) map[string]interface{} {
	if len(values) == 0 {
		return map[string]interface{}{"samples": 0}
	}
	sort.Float64s(values)
	d := map[string]interface{}{"samples": len(values)}
	for name, q := range map[string]float64{"p50_ms": .5, "p90_ms": .9, "p95_ms": .95, "p99_ms": .99} {
		d[name] = values[int(math.Ceil(q*float64(len(values))))-1]
	}
	return d
}

func main() {
	base := flag.String("base", "", "base including channel/v1, e.g. https://gateway/workbuddy/v1")
	model := flag.String("model", "", "model ID")
	effort := flag.String("effort", "", "explicit reasoning effort, empty uses server default")
	n := flag.Int("n", 20, "measured requests")
	concurrency := flag.Int("c", 1, "closed-loop concurrency")
	warm := flag.Int("warmup", 1, "excluded warmup requests")
	budget := flag.Int("max-tokens", 1024, "explicit output budget including reasoning")
	timeout := flag.Duration("timeout", 90*time.Second, "each request total deadline")
	output := flag.String("output", "", "optional JSON report path; no credentials or response text")
	flag.Parse()
	u, err := url.Parse(*base)
	key := os.Getenv("API_BENCH_KEY")
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || key == "" || *model == "" || *n < 1 || *n > 10000 || *concurrency < 1 || *concurrency > 128 || *warm < 0 || *warm > 100 || *budget < 1 || *budget > 131072 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "set API_BENCH_KEY and valid -base, -model, -n, -c, -max-tokens, -timeout")
		os.Exit(2)
	}
	endpoint := strings.TrimRight(*base, "/") + "/chat/completions"
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 128, MaxIdleConnsPerHost: *concurrency, MaxConnsPerHost: *concurrency, IdleConnTimeout: 90 * time.Second}}
	for i := 0; i < *warm; i++ {
		probe(client, endpoint, key, *model, *effort, *budget, *timeout)
	}
	rows := make([]result, *n)
	jobs := make(chan int)
	var wg sync.WaitGroup
	started := time.Now()
	for i := 0; i < min(*concurrency, *n); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				rows[j] = probe(client, endpoint, key, *model, *effort, *budget, *timeout)
				rows[j].Index = j
			}
		}()
	}
	for j := range rows {
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	seconds := time.Since(started).Seconds()
	client.CloseIdleConnections()
	var e2e, generated, visible, gaps []float64
	httpOK, protocolOK, answers, truncated, reasoningOnly := 0, 0, 0, 0, 0
	tokens, usageSamples, correctAnswers := 0, 0, 0
	for _, r := range rows {
		e2e = append(e2e, r.E2EMS)
		if r.HTTPStatus == 200 {
			httpOK++
		}
		if r.Error == "" && r.Done && r.Finish != "" {
			protocolOK++
			if r.FirstGeneratedMS != nil {
				generated = append(generated, *r.FirstGeneratedMS)
			}
			if r.FirstVisibleMS != nil {
				visible = append(visible, *r.FirstVisibleMS)
			}
			gaps = append(gaps, r.DeltaGapsMS...)
			if r.VisibleChars > 0 {
				answers++
			}
			if r.ReasoningChars > 0 && r.VisibleChars == 0 && !r.ToolOutput {
				reasoningOnly++
			}
			if r.Finish == "length" || r.Finish == "max_tokens" {
				truncated++
			}
			if r.AnswerCorrect {
				correctAnswers++
			}
			if r.UsageReported {
				usageSamples++
				tokens += r.OutputTokens
			}
		}
	}
	var tokenRate interface{}
	if usageSamples > 0 {
		tokenRate = float64(tokens) / seconds
	}
	report := map[string]interface{}{"model": *model, "concurrency": *concurrency, "samples": *n, "warmup_excluded": *warm, "max_tokens": *budget, "http_success": httpOK, "protocol_success": protocolOK, "visible_answers": answers, "reasoning_only": reasoningOnly, "output_truncated": truncated, "elapsed_seconds": seconds, "completed_rps": float64(protocolOK) / seconds, "usage_samples": usageSamples, "correct_answers": correctAnswers, "reported_output_tokens_per_second": tokenRate, "e2e_all": distribution(e2e), "first_generated_protocol_success": distribution(generated), "first_visible_protocol_success": distribution(visible), "visible_delta_gap": distribution(gaps), "notes": []string{"closed-loop workload, not maximum capacity", "delta gaps are not token ITL; reported output tokens may include reasoning", "HTTP success, protocol success, visible answers and truncation are distinct", "P99 needs sufficient samples; fewer than 100 requests is exploratory", "client timings include network/CDN; correlate request IDs with gateway and Caddy logs"}, "results": rows}
	raw, _ := json.MarshalIndent(report, "", "  ")
	if *output != "" {
		if err := os.WriteFile(*output, append(raw, '\n'), 0600); err != nil {
			fmt.Fprintln(os.Stderr, "could not save report")
			os.Exit(1)
		}
	}
	fmt.Println(string(raw))
}
