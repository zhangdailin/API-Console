// warp-probe is an independent, memory-only operational test, not a gateway channel.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func report(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
func main() {
	if err := run(); err != nil {
		report(map[string]any{"status": "failed", "error": err.Error()})
		os.Exit(1)
	}
}
func validateProbeModes(live, start, oauth bool, model string) error {
	modes := 0
	for _, enabled := range []bool{live, start, oauth} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("select exactly one probe mode")
	}
	if model != "" && !live {
		return errors.New("model selection requires --live")
	}
	return nil
}

func validateGenerationPolicy(live, generate bool, model string, maxCredits float64) error {
	if math.IsNaN(maxCredits) || math.IsInf(maxCredits, 0) || maxCredits < 0 {
		return errors.New("credit ceiling must be finite and nonnegative")
	}
	if generate && (!live || model == "") {
		return errors.New("generation requires --live --generate --model with an exact account-observed model")
	}
	if !generate && (model != "" || maxCredits != 0) {
		return errors.New("model and credit ceiling require explicit --generate")
	}
	if generate {
		name := strings.ToLower(strings.TrimSpace(model))
		if !modelID.MatchString(model) || name == "auto" || strings.HasPrefix(name, "auto-") || strings.Contains(name, "cli-agent-auto") || strings.Contains(name, "computer-use-agent") {
			return errors.New("automatic routing and subagent model aliases are disabled in the minimal probe")
		}
	}
	return nil
}

func checkReportedBudget(usage probeUsage, maxCredits float64) error {
	if maxCredits <= 0 {
		return nil
	}
	if usage.InferenceCredits == nil || usage.PlatformCredits == nil {
		return errors.New("credit budget unverified: upstream did not provide both request credit components")
	}
	if *usage.InferenceCredits+*usage.PlatformCredits > maxCredits {
		return errors.New("reported credit ceiling exceeded; charges may already have been incurred")
	}
	// Present components can still be a partial lower bound. A below-ceiling
	// observation is not evidence of a complete bill or a precharge limit.
	return errors.New("observed credits below ceiling, but full bill completeness is unverified")
}

func run() error {
	live := flag.Bool("live", false, "Authorize login and catalog only; never generate without --generate and --model")
	generate := flag.Bool("generate", false, "Authorize exactly one potentially billed text generation")
	maxCredits := flag.Float64("max-reported-credits", 0, "Post-response credit ceiling; cannot prevent charges before final usage arrives")
	oauthOnly := flag.Bool("oauth-login", false, "Test real browser approval and OAuth token polling only; no generation")
	startOnly := flag.Bool("auth-start-only", false, "Request one real device authorization without token exchange or generation")
	chosen := flag.String("model", "", "Select an exact model returned by this account's catalog")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments; never pass credentials as arguments")
	}
	if err := validateProbeModes(*live, *startOnly, *oauthOnly, *chosen); err != nil {
		return err
	}
	if err := validateGenerationPolicy(*live, *generate, *chosen, *maxCredits); err != nil {
		return err
	}
	if !*live && !*startOnly && !*oauthOnly {
		report(map[string]any{"status": "preflight", "firebase_key_configured": strings.TrimSpace(os.Getenv("ORCHIDS_WARP_FIREBASE_API_KEY")) != "", "credentials_saved": false, "network_used": false, "next": "Use --oauth-login for browser approval, --live for catalog only, or --live --generate --model <exact-id> for one billed generation"})
		return nil
	}
	key := strings.TrimSpace(os.Getenv("ORCHIDS_WARP_FIREBASE_API_KEY"))
	if *live && key == "" {
		return errors.New("ORCHIDS_WARP_FIREBASE_API_KEY must be configured privately in the local environment; do not send it in chat")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 17*time.Minute)
	defer timeout()
	c := newProbeClient(key)
	d, err := c.start(ctx)
	if err != nil {
		return fmt.Errorf("device_start: %w", err)
	}
	verify := d.VerificationComplete
	if verify == "" {
		verify = d.VerificationURI
	}
	report(map[string]any{"stage": "device_authorization", "status": "pending", "verification_url": verify, "user_code": d.UserCode, "expires_at": time.Now().Add(time.Duration(d.ExpiresIn) * time.Second).UTC().Format(time.RFC3339), "credentials_saved": false})
	if *startOnly {
		report(map[string]any{"status": "device_endpoint_passed", "token_exchange_tested": false, "generation_tested": false})
		return nil
	}
	if *oauthOnly {
		custom, err := c.awaitCustom(ctx, d)
		d.DeviceCode = ""
		if err != nil {
			return fmt.Errorf("oauth_login: %w", err)
		}
		if custom == "" {
			return errors.New("OAuth did not produce an authorization result")
		}
		custom = ""
		report(map[string]any{"stage": "oauth_login", "status": "passed", "credentials_saved": false, "firebase_exchange_tested": false, "generation_tested": false})
		return nil
	}
	token, err := c.await(ctx, d)
	d.DeviceCode = ""
	if err != nil {
		return fmt.Errorf("device_exchange: %w", err)
	}
	report(map[string]any{"stage": "authentication", "status": "passed", "credentials_saved": false})
	models, preferred, err := c.models(ctx, token)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	report(map[string]any{"stage": "catalog", "status": "passed", "models": models, "upstream_default": preferred, "generation_authorized": *generate})
	if !*generate {
		token = ""
		report(map[string]any{"status": "catalog_only_passed", "generation_tested": false, "credentials_saved": false})
		return nil
	}
	model := ""
	if *chosen != "" {
		found := false
		for _, m := range models {
			if m == *chosen {
				found = true
				model = m
				break
			}
		}
		if !found {
			return errors.New("requested model was not present in this account's enabled catalog")
		}
	}
	report(map[string]any{"stage": "catalog", "status": "passed", "models": models, "selected_model": model})
	generationCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	result, err := c.generateObserved(generationCtx, token, model, models)
	token = ""
	report(map[string]any{"stage": "generation_observation", "usage": result.Usage, "budget_enforcement": "post_response_only", "local_deadline_seconds": 30, "local_output_byte_limit": 4096})
	if err != nil {
		return fmt.Errorf("generation: %w", err)
	}
	if err := checkReportedBudget(result.Usage, *maxCredits); err != nil {
		return err
	}
	if !result.Complete || result.TextBytes == 0 || result.ToolRequested {
		return errors.New("generation did not produce a complete nonempty text-only answer")
	}
	report(map[string]any{"status": "passed", "scope": "one fresh text-only generation; not tools or multi-turn support", "model": model, "events": result.Events, "text_bytes": result.TextBytes, "finished": result.Complete, "tool_requested": result.ToolRequested, "credentials_saved": false})
	return nil
}
