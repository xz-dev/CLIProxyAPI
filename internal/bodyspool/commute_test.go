package bodyspool_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func dataURL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

// corpus covers escapes, unicode, surrogates, duplicate keys, big integers, 1e3, deep nesting,
// data URLs, signatures, affinity tags and large inline text (which stays inline).
func responsesCorpus() map[string]string {
	a, b, big := dataURL(200<<10), dataURL(90<<10), strings.Repeat("long tool output line\\n", 4000)
	deep := strings.Repeat(`{"k":`, 40) + `"v"` + strings.Repeat(`}`, 40)
	return map[string]string{
		"image-heavy": fmt.Sprintf(`{"model":"m","stream":true,"instructions":"be brief \u00e9 \ud83d\ude00 \"q\" <&>","input":[`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":%q,"detail":"high"}]},`+
			`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"ls\",\"n\":12345678901234567890,\"x\":1e3}"},`+
			`{"type":"function_call_output","call_id":"c1","output":%q},`+
			`{"type":"reasoning","encrypted_content":"gAAAA-signature-blob","summary":[]},`+
			`{"type":"message","role":"user","content":[{"type":"input_image","image_url":%q},{"type":"input_file","filename":"a.pdf","file_data":%q}]}],`+
			`"metadata":{"affinity":"tag-1","deep":%s},"prompt_cache_key":"pck","prompt_cache_key":"dup-last"}`, a, big, b, a, deep),
		"text-only": `{"model":"m","input":"` + big + `"}`,
		"escaped-media": `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:text/plain,` +
			strings.Repeat(`a\/b \u00e9 <&> \ud83d\ude00 \ud800 \n\t \"q\" `, 3000) + "\xff" + `"}]}]}`,
	}
}

func chatCorpus() map[string]string {
	a := dataURL(150 << 10)
	return map[string]string{
		"chat-image": fmt.Sprintf(`{"model":"m","stream":true,"messages":[{"role":"system","content":"sys \u00e9"},`+
			`{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":%q,"detail":"auto"}}]},`+
			`{"role":"assistant","content":"a cat","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},`+
			`{"role":"tool","tool_call_id":"t1","content":"ok"},{"role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}]}`, a, a),
	}
}

func ingest(t *testing.T, body string) ([]byte, *bodyspool.Spool) {
	t.Helper()
	sk, sp, err := bodyspool.Ingest(strings.NewReader(body), bodyspool.Options{Dir: t.TempDir(), Threshold: 64 << 10, Hop: "cpa"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sk, sp
}

func fingerprints(from sdktranslator.Format, payload []byte) []string {
	var out []string
	for _, turn := range cliproxysession.ExtractCanonicalTurns(from, payload) {
		out = append(out, cliproxysession.FastTurnFingerprint(turn))
	}
	return out
}

// TestTranslatorsCommuteWithSpool: translate(body) == splice(translate(skeleton)) for every
// covered target, and the session identity and turn fingerprints are unchanged.
func TestTranslatorsCommuteWithSpool(t *testing.T) {
	sets := map[sdktranslator.Format]map[string]string{
		sdktranslator.FormatOpenAIResponse: responsesCorpus(),
		sdktranslator.FormatOpenAI:         chatCorpus(),
	}
	if p := os.Getenv("BODYSPOOL_BODY"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sets[sdktranslator.FormatOpenAIResponse][p] = string(raw)
	}
	targets := []sdktranslator.Format{sdktranslator.FormatCodex, sdktranslator.FormatClaude, sdktranslator.FormatOpenAI}
	for from, corpus := range sets {
		for name, body := range corpus {
			sk, sp := ingest(t, body)
			if (name == "image-heavy" || name == "chat-image") && sp.Blobs() == 0 {
				t.Fatalf("%s: nothing spooled", name)
			}
			for _, to := range targets {
				want := sdktranslator.TranslateRequest(from, to, "m", []byte(body), true)
				gotSk := sdktranslator.TranslateRequest(from, to, "m", bytes.Clone(sk), true)
				got, err := sp.Materialize(gotSk, 0, "test")
				if err != nil {
					t.Fatalf("%s %s->%s: %v", name, from, to, err)
				}
				if !bytes.Equal(got, want) {
					i := 0
					for i < len(got) && i < len(want) && got[i] == want[i] {
						i++
					}
					t.Fatalf("%s %s->%s: spliced output differs at %d (len %d vs %d)", name, from, to, i, len(got), len(want))
				}
				if bytes.Contains(gotSk, []byte(bodyspool.MarkerPrefix)) && sp.Blobs() == 0 {
					t.Fatalf("%s: marker without blob", name)
				}
				t.Logf("%s %s->%s: upstream %d bytes, skeleton %d bytes, blobs %d", name, from, to, len(want), len(gotSk), sp.Blobs())
			}
			wantInfo, okWant := cliproxysession.ExtractSessionInfo(nil, []byte(body), nil)
			gotInfo, okGot := cliproxysession.ExtractSessionInfo(nil, sk, nil)
			if okWant != okGot || !reflect.DeepEqual(wantInfo, gotInfo) {
				t.Fatalf("%s: session info changed: %+v vs %+v", name, wantInfo, gotInfo)
			}
			if w, g := cliproxysession.DeriveID(from, []byte(body), "scope"), cliproxysession.DeriveID(from, sk, "scope"); w != g {
				t.Fatalf("%s: derived session id changed: %q vs %q", name, w, g)
			}
			if w, g := fingerprints(from, []byte(body)), fingerprints(from, sk); strings.Join(w, ",") != strings.Join(g, ",") {
				t.Fatalf("%s: turn fingerprints changed:\n%v\n%v", name, w, g)
			}
		}
	}
}
