package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recorder struct {
	triggers int
	seen     []string
}

func newTestHandler(rec *recorder) http.Handler {
	h := &Handler{
		Secrets: map[string][]byte{"cloud": []byte(fixtureSecret)},
		Trigger: func() { rec.triggers++ },
		Observe: func(source, outcome string) { rec.seen = append(rec.seen, source+"/"+outcome) },
		Now:     func() time.Time { return fixtureNow },
	}
	return h.Mux()
}

func TestWebhookHandler(t *testing.T) {
	testBody := `{"event":"test","project":{},"timestamp":1791200000000}`
	otherBody := `{"event":"secrets.rotation-failed","project":{},"timestamp":1791200000000}`

	for _, tc := range []struct {
		name, method, path, header, body string
		wantCode                         int
		wantSeen                         string
		wantTrigger                      bool
	}{
		{"secrets.modified starts a pass", http.MethodPost, "/webhook/cloud",
			"t=1791200000000;" + fixtureSig, fixtureBody, http.StatusAccepted, "cloud/accepted", true},
		{"test event starts nothing", http.MethodPost, "/webhook/cloud",
			sign(fixtureSecret, testBody, fixtureTS), testBody, http.StatusOK, "cloud/test", false},
		{"other events are ignored", http.MethodPost, "/webhook/cloud",
			sign(fixtureSecret, otherBody, fixtureTS), otherBody, http.StatusNoContent, "cloud/ignored", false},
		{"unsigned", http.MethodPost, "/webhook/cloud", "", fixtureBody, http.StatusUnauthorized, "cloud/unsigned", false},
		{"bad signature", http.MethodPost, "/webhook/cloud",
			sign("wrong", fixtureBody, fixtureTS), fixtureBody, http.StatusUnauthorized, "cloud/bad_signature", false},
		{"too large", http.MethodPost, "/webhook/cloud",
			"t=1791200000000;" + fixtureSig, strings.Repeat("x", MaxBody+1), http.StatusRequestEntityTooLarge, "cloud/too_large", false},
		// The caller-chosen segment must not reach a label.
		{"unknown source", http.MethodPost, "/webhook/attacker-chosen",
			"t=1791200000000;" + fixtureSig, fixtureBody, http.StatusNotFound, "/unknown_source", false},
		{"wrong method", http.MethodGet, "/webhook/cloud", "", "", http.StatusMethodNotAllowed, "", false},
		{"other path", http.MethodPost, "/metrics", "", "", http.StatusNotFound, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.header != "" {
				req.Header.Set("X-Infisical-Signature", tc.header)
			}
			w := httptest.NewRecorder()
			newTestHandler(rec).ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if tc.wantCode != http.StatusMethodNotAllowed && tc.wantCode != http.StatusNotFound && w.Body.Len() != 0 {
				t.Errorf("response body = %q, want none", w.Body.String())
			}
			if got := strings.Join(rec.seen, ","); got != tc.wantSeen {
				t.Errorf("observed %q, want %q", got, tc.wantSeen)
			}
			if (rec.triggers > 0) != tc.wantTrigger || rec.triggers > 1 {
				t.Errorf("triggered %d times, want trigger=%v", rec.triggers, tc.wantTrigger)
			}
		})
	}
}
