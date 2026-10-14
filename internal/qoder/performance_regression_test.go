package qoder

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestCatalogCacheRefreshesOnlyOnSnapshotChange(t *testing.T) {
	c := NewFromAccount(signedTestAccount(), nil)
	first := c.loadCatalog()
	testutil.Equal(t, c.loadCatalog(), first)
	c.stateMu.Lock()
	c.account.QoderModelIDs = []string{"new-key\tNew Model"}
	c.stateMu.Unlock()
	next := c.loadCatalog()
	testutil.False(t, next == first || next.Len() != 1, "changed catalog not refreshed")
	_, err := first.Resolve("Qwen3.7-Max")
	testutil.NoError(t, err)
}

func TestPackedBearerPreservesPayloadAndSignatureIncludingLargePayload(t *testing.T) {
	for _, info := range []string{"encrypted-info", "中文 <>&\n\"", strings.Repeat("large", 2000)} {
		got, err := buildCOSYAuthorization("request", info, "version", "key", "123", []byte("encoded-body"), "/path")
		testutil.NoError(t, err)
		parts := strings.Split(got, ".")
		testutil.Equal(t, len(parts), 3)
		testutil.Equal(t, parts[0], "Bearer COSY")
		raw, err := base64.StdEncoding.DecodeString(parts[1])
		testutil.NoError(t, err, "payload is not standard base64: %v")
		var fields map[string]string
		testutil.NoError(t, json.Unmarshal(raw, &fields))
		wantFields := map[string]string{"version": "v1", "requestId": "request", "info": info, "cosyVersion": "version", "ideVersion": ""}
		testutil.Equal(t, len(fields), len(wantFields))
		for key, want := range wantFields {
			value, ok := fields[key]
			testutil.Falsef(t, !ok || value != want, "payload field %q = %q (present=%t), want %q", key, value, ok, want)
		}
		// Pin wire order and JSON escaping without recreating a payload builder.
		quotedInfo, err := json.Marshal(info)
		testutil.NoError(t, err)
		wantRaw := `{"version":"v1","requestId":"request","info":` + string(quotedInfo) + `,"cosyVersion":"version","ideVersion":""}`
		testutil.Equal(t, string(raw), wantRaw)
		wantSignature := fmt.Sprintf("%x", md5.Sum([]byte(parts[1]+"\nkey\n123\nencoded-body\n/path")))
		testutil.Equal(t, parts[2], wantSignature)
	}
}

func TestBatchedUUIDsMatchIndependentEntropyReads(t *testing.T) {
	data := make([]byte, 48)
	for i := range data {
		data[i] = byte(i)
	}
	r, s, c, err := newChatUUIDs(bytes.NewReader(data))
	testutil.NoError(t, err)
	reader := bytes.NewReader(data)
	for _, got := range []string{r, s, c} {
		want, err := newUUID(reader)
		testutil.False(t, err != nil || want != got, "UUID format or entropy independence changed")
	}
	_, _, _, err = newChatUUIDs(bytes.NewReader(data[:47]))
	testutil.Error(t, err)
}

func TestPooledJSONEncodingPreservesWireAndOwnership(t *testing.T) {
	values := []interface{}{
		map[string]interface{}{"prompt": "中文 <>&\n\"", "tools": []interface{}{}, "empty": nil},
		referenceChatContext(upstream.UpstreamRequest{Prompt: "中文"}, modelEntry{Key: "key", IsReasoning: true}),
		map[string]interface{}{"large": string(bytes.Repeat([]byte("x"), 70000))},
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for _, value := range values {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Error(err)
					return
				}
				got, err := marshalEncodedBody(value)
				if err != nil || !bytes.Equal(got, EncodeBody(raw)) {
					t.Error("pooled encoding changed wire")
					return
				}
				_, _ = marshalEncodedBody(map[string]interface{}{"other": "reuses buffer"})
				testutil.CheckFalse(t, !bytes.Equal(got, EncodeBody(raw)), "returned body aliases pool")
			}
		})
	}
	wg.Wait()
	_, err := marshalEncodedBody(make(chan int))
	testutil.Error(t, err)
}

func TestPackedAuthHeaderValuesDoNotAliasOnAdd(t *testing.T) {
	c := NewFromAccount(signedTestAccount(), nil)
	req, _ := http.NewRequest(http.MethodPost, "http://mock.invalid/chat", nil)
	testutil.NoError(t, c.applyAuthHeaders(req, credsOf(signedTestAccount()), RuntimeFields{EncryptUserInfo: "info", Key: "key"}, "request", "model", "system", "body", "/chat"))
	want := req.Header.Clone()
	req.Header.Add("Accept", "additional")
	for key, values := range want {
		testutil.Falsef(t, key != "Accept" && req.Header.Get(key) != values[0], "header %s overwritten", key)
	}
}
