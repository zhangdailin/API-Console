package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/web"
)

func TestOpsFlowRendersVisibleRegistryChannels(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err)
	for _, target := range []string{"/?tab=ops", "/"} {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			err := renderer.RenderIndex(recorder, httptest.NewRequest(http.MethodGet, target, nil), &config.Config{AdminPath: "/console"}, nil)
			testutil.NoError(t, err)
			page := recorder.Body.String()
			root, err := html.Parse(strings.NewReader(page))
			testutil.NoError(t, err)
			var visible []channel.Definition
			for _, definition := range channel.All() {
				if definition.FlowVisible {
					visible = append(visible, definition)
				}
			}
			assertFlowProviders(t, root, visible)
			testutil.CheckContain(t, page, `id="opsNode-warp"`)
			testutil.CheckNotContain(t, page, `id="opsNode-doubao"`)
			testutil.CheckNotContain(t, page, "ZgotmplZ")

			scripts := []string{"/js/provider-registry.js?v=", "/js/ops-flow-model.js?v=", "/js/ops-flow.js?v="}
			previous := -1
			for _, script := range scripts {
				position := strings.Index(page, "/console"+script)
				if position <= previous {
					t.Fatalf("missing or incorrectly ordered script %s", script)
				}
				previous = position
			}
		})
	}
}

// A different registry projection must not leave hardcoded nodes, labels or SVG
// elements behind. A fifth channel also exercises the CSS count without casts.
func TestOpsFlowTemplateUsesProvidedDefinitions(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err)
	providers := make([]channel.Definition, 5)
	for i := range providers {
		providers[i] = channel.Definition{ID: channel.ID(fmt.Sprintf("future-%d", i)), Label: fmt.Sprintf("Future & Channel %d", i)}
	}
	var output bytes.Buffer
	testutil.NoError(t, renderer.templates.ExecuteTemplate(&output, "page-ops", &PageData{
		AdminPath: "/admin", ActiveTab: "ops", FlowProviders: providers,
	}))
	root, err := html.Parse(&output)
	testutil.NoError(t, err)
	assertFlowProviders(t, root, providers)
}

func assertFlowProviders(t *testing.T, root *html.Node, providers []channel.Definition) {
	t.Helper()
	ids := make(map[string][]*html.Node)
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if id := flowAttribute(node, "id"); id != "" {
			ids[id] = append(ids[id], node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	for id, nodes := range ids {
		if len(nodes) != 1 {
			t.Fatalf("DOM id %q occurs %d times", id, len(nodes))
		}
	}
	one := func(id string) *html.Node {
		t.Helper()
		if len(ids[id]) != 1 {
			t.Fatalf("DOM id %q occurs %d times, want 1", id, len(ids[id]))
		}
		return ids[id][0]
	}
	testutil.Equal(t, flowAttribute(one("flowBoard"), "style"), fmt.Sprintf("--flow-channel-count:%d", len(providers)))
	var options []string
	for child := one("flowChannel").FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == "option" {
			options = append(options, flowAttribute(child, "value")+":"+flowNodeText(child))
		}
	}
	wantOptions := []string{":全部渠道"}
	for _, provider := range providers {
		id := string(provider.ID)
		wantOptions = append(wantOptions, id+":"+provider.Label)
		for _, prefix := range []string{"opsNode-", "opsNodeQps-", "opsNodeTps-", "flowActive-", "flowState-", "flowPool-", "flowCapacity-", "flowCapacityBar-", "opsWire-", "flowBar-"} {
			one(prefix + id)
		}
		button := one("opsNode-" + id)
		testutil.Equal(t, button.Data, "button")
		testutil.Equal(t, flowAttribute(button, "data-flow-node"), id)
		if !strings.Contains(flowNodeText(button), provider.Label) {
			t.Fatalf("channel %s does not display label %q", id, provider.Label)
		}
	}
	if !reflect.DeepEqual(options, wantOptions) {
		t.Fatalf("flow options = %v, want registry projection %v", options, wantOptions)
	}
	wantWires := []string{"chat", "responses", "messages"}
	for _, provider := range providers {
		wantWires = append(wantWires, string(provider.ID))
	}
	var wires, bars []string
	for child := one("flowWires").FirstChild; child != nil; child = child.NextSibling {
		wire := flowAttribute(child, "data-wire")
		if wire == "" {
			continue
		}
		switch child.Data {
		case "g":
			wires = append(wires, wire)
			var paths []string
			for path := child.FirstChild; path != nil; path = path.NextSibling {
				if path.Data == "path" {
					paths = append(paths, flowAttribute(path, "class"))
				}
			}
			if !reflect.DeepEqual(paths, []string{"flow-band", "flow-wire-in", "flow-wire-out"}) {
				t.Fatalf("wire %s paths = %v", wire, paths)
			}
		case "rect":
			bars = append(bars, wire)
		}
	}
	if !reflect.DeepEqual(wires, wantWires) || !reflect.DeepEqual(bars, wantWires) {
		t.Fatalf("SVG wires = %v, bars = %v; want %v", wires, bars, wantWires)
	}
	var nodes []string
	for _, matches := range ids {
		if strings.Contains(" "+flowAttribute(matches[0], "class")+" ", " flow-channel ") {
			nodes = append(nodes, flowAttribute(matches[0], "data-flow-node"))
		}
	}
	testutil.Equal(t, len(nodes), len(providers))
}

func TestOpsFlowGeneratedRegistryMatchesChannelDefinitions(t *testing.T) {
	response := httptest.NewRecorder()
	web.StaticHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/js/provider-registry.js", nil))
	testutil.Equal(t, response.Code, http.StatusOK)
	rows := regexp.MustCompile(`Object\.freeze\((\{[^\n]+\})\),`).FindAllStringSubmatch(response.Body.String(), -1)
	var generated []channel.Definition
	for _, row := range rows {
		var definition channel.Definition
		testutil.NoError(t, json.Unmarshal([]byte(row[1]), &definition))
		generated = append(generated, definition)
	}
	if !reflect.DeepEqual(generated, channel.All()) {
		t.Fatalf("generated browser registry differs from channel definitions: got %+v, want %+v", generated, channel.All())
	}
}

func flowAttribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}

func flowNodeText(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var text strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		text.WriteString(flowNodeText(child))
	}
	return text.String()
}
