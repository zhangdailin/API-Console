package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"orchids-api/internal/store"
)

// The paid Grok Web plans share a weekly credit pool, read from the same
// gRPC-web endpoint the web app uses. A paid account reports a usage rate and
// precise period bounds; a Free account reports a coarse period and no rate.
const webWeeklyCreditsPath = "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"

// WeeklyCredits is one read of the shared weekly pool.
type WeeklyCredits struct {
	Paid         bool
	UsagePercent float64
	PeriodEnd    time.Time
}

// GetWeeklyCredits reads the weekly credit pool of an SSO account.
func (c *Client) GetWeeklyCredits(ctx context.Context, token string) (WeeklyCredits, error) {
	reqURL := c.baseURL() + webWeeklyCreditsPath
	headers := c.headersFor(ctx, token, http.MethodPost, reqURL)
	headers.Set("Content-Type", "application/grpc-web+proto")
	headers.Set("x-grpc-web", "1")
	headers.Set("x-user-agent", "connect-es/2.1.1")
	headers.Del("x-xai-request-id")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return WeeklyCredits{}, err
	}
	req.Header = headers
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return WeeklyCredits{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return WeeklyCredits{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return WeeklyCredits{}, fmt.Errorf("grok weekly credits status=%d", resp.StatusCode)
	}
	return parseWeeklyCredits(body)
}

func parseWeeklyCredits(body []byte) (WeeklyCredits, error) {
	if len(body) < 5 || body[0] != 0 {
		return WeeklyCredits{}, fmt.Errorf("grok weekly credits: no data frame")
	}
	size := int(binary.BigEndian.Uint32(body[1:5]))
	if size > len(body)-5 {
		return WeeklyCredits{}, fmt.Errorf("grok weekly credits: truncated frame")
	}
	config, ok := protoField(body[5:5+size], 1)
	if !ok {
		return WeeklyCredits{}, fmt.Errorf("grok weekly credits: missing config")
	}
	var out WeeklyCredits
	var usagePresent, precise bool
	var start, end time.Time
	for len(config) > 0 {
		num, typ, n := protowire.ConsumeTag(config)
		if n < 0 {
			return WeeklyCredits{}, fmt.Errorf("grok weekly credits: bad tag")
		}
		config = config[n:]
		switch {
		case num == 1 && typ == protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(config)
			if m < 0 {
				return WeeklyCredits{}, fmt.Errorf("grok weekly credits: bad usage")
			}
			out.UsagePercent = float64(math.Float32frombits(v))
			usagePresent = true
			config = config[m:]
		case (num == 4 || num == 5) && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(config)
			if m < 0 {
				return WeeklyCredits{}, fmt.Errorf("grok weekly credits: bad period")
			}
			ts, nanos := protoTimestamp(v)
			if nanos != 0 {
				precise = true
			}
			if num == 4 {
				start = ts
			} else {
				end = ts
			}
			config = config[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, config)
			if m < 0 {
				return WeeklyCredits{}, fmt.Errorf("grok weekly credits: bad field")
			}
			config = config[m:]
		}
	}
	if math.IsNaN(out.UsagePercent) || out.UsagePercent < 0 || out.UsagePercent > 100 {
		usagePresent, out.UsagePercent = false, 0
	}
	// Free accounts return a coarse entitlement period without a rate; an
	// unused paid pool omits the rate too but keeps precise period bounds.
	out.Paid = usagePresent || precise
	if end.After(start) {
		out.PeriodEnd = end.UTC()
	}
	return out, nil
}

func protoField(message []byte, target protowire.Number) ([]byte, bool) {
	for len(message) > 0 {
		num, typ, n := protowire.ConsumeTag(message)
		if n < 0 {
			return nil, false
		}
		message = message[n:]
		if num == target && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(message)
			return v, m >= 0
		}
		m := protowire.ConsumeFieldValue(num, typ, message)
		if m < 0 {
			return nil, false
		}
		message = message[m:]
	}
	return nil, false
}

func protoTimestamp(message []byte) (time.Time, int64) {
	var seconds, nanos int64
	for len(message) > 0 {
		num, typ, n := protowire.ConsumeTag(message)
		if n < 0 {
			break
		}
		message = message[n:]
		if typ == protowire.VarintType && (num == 1 || num == 2) {
			v, m := protowire.ConsumeVarint(message)
			if m < 0 {
				break
			}
			if num == 1 {
				seconds = int64(v)
			} else {
				nanos = int64(v)
			}
			message = message[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, message)
		if m < 0 {
			break
		}
		message = message[m:]
	}
	return time.Unix(seconds, nanos), nanos
}

// weeklyWindow projects a weekly read onto the stored quota window. Only a
// paid pool is stored: a Free account has no weekly allowance to show.
func weeklyWindow(credits WeeklyCredits) store.GrokQuotaWindow {
	if !credits.Paid {
		return store.GrokQuotaWindow{}
	}
	return store.GrokQuotaWindow{
		Limit: 100, HasLimit: true,
		Remaining: max(0, 100-credits.UsagePercent), HasRemaining: true,
		UsagePercent: credits.UsagePercent, HasUsage: true,
		ResetAt: credits.PeriodEnd,
	}
}
