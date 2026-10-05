package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixtureBody is byte for byte what Infisical's JSON.stringify produces for a
// secrets.modified event: insertion key order and <>& unescaped, both of which
// a json.Marshal round trip would change. fixtureSig was computed outside Go,
// by openssl dgst -sha256 -hmac and by node's crypto.createHmac, which agree.
const (
	fixtureSecret = "fixture-webhook-secret"
	fixtureBody   = `{"event":"secrets.modified","project":{"workspaceId":"9f1c2b7e-0d4a-4e8b-a3c1-5b6d7e8f9a0b","projectId":"9f1c2b7e-0d4a-4e8b-a3c1-5b6d7e8f9a0b","projectName":"Home <lab> & co","environment":"prod","environmentName":"Production","secretPath":"/apps/arr-stack","changedBy":"infisical-mirror","changedByActorType":"identity"},"timestamp":1791200000000}`
	fixtureSig    = "b59dfb96f21170ec4775c04556fa85c73ccb8fddbaba53fff3c8527513bdaa2c"
	fixtureTS     = 1791200000000
)

var fixtureNow = time.UnixMilli(fixtureTS).Add(30 * time.Second)

// sign signs a body as Infisical does, for cases the fixture does not cover.
func sign(secret, body string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "t=" + strconv.FormatInt(ts, 10) + ";" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyAcceptsInfisicalsOwnBytes(t *testing.T) {
	ev, err := Verify([]byte(fixtureSecret), "t=1791200000000;"+fixtureSig, []byte(fixtureBody), fixtureNow)
	if err != nil {
		t.Fatalf("Verify rejected a request signed exactly as Infisical signs it: %v", err)
	}
	want := Event{
		Type:        EventSecretsModified,
		ProjectID:   "9f1c2b7e-0d4a-4e8b-a3c1-5b6d7e8f9a0b",
		Environment: "prod",
		SecretPath:  "/apps/arr-stack",
	}
	if ev != want {
		t.Errorf("Verify = %+v, want %+v", ev, want)
	}
}

func TestVerifyRejects(t *testing.T) {
	stale := `{"event":"secrets.modified","project":{},"timestamp":1791100000000}`
	future := `{"event":"secrets.modified","project":{},"timestamp":1791300000000}`
	noStamp := `{"event":"secrets.modified","project":{}}`
	noEvent := `{"project":{},"timestamp":1791200000000}`

	for _, tc := range []struct {
		name, secret, header, body string
		want                       error
	}{
		{"no header", fixtureSecret, "", fixtureBody, ErrUnsigned},
		{"wrong secret", "another-secret", "t=1791200000000;" + fixtureSig, fixtureBody, ErrBadSignature},
		{"tampered body", fixtureSecret, "t=1791200000000;" + fixtureSig,
			strings.Replace(fixtureBody, "/apps/arr-stack", "/apps/other", 1), ErrBadSignature},
		{"header stamp differs from the signed one", fixtureSecret, "t=1791200000001;" + fixtureSig, fixtureBody, ErrMalformed},
		{"uppercase hex", fixtureSecret, "t=1791200000000;" + strings.ToUpper(fixtureSig), fixtureBody, ErrMalformed},
		{"short hex", fixtureSecret, "t=1791200000000;" + fixtureSig[:62], fixtureBody, ErrMalformed},
		{"non-hex", fixtureSecret, "t=1791200000000;" + strings.Repeat("z", 64), fixtureBody, ErrMalformed},
		{"Stripe-style comma header", fixtureSecret, "t=1791200000000,v1=" + fixtureSig, fixtureBody, ErrMalformed},
		{"no t= prefix", fixtureSecret, "1791200000000;" + fixtureSig, fixtureBody, ErrMalformed},
		{"signed stamp", fixtureSecret, "t=-1;" + fixtureSig, fixtureBody, ErrMalformed},
		{"empty stamp", fixtureSecret, "t=;" + fixtureSig, fixtureBody, ErrMalformed},
		{"trailing field", fixtureSecret, "t=1791200000000;" + fixtureSig + ";x", fixtureBody, ErrMalformed},
		{"stale", fixtureSecret, sign(fixtureSecret, stale, 1791100000000), stale, ErrStale},
		{"from the future", fixtureSecret, sign(fixtureSecret, future, 1791300000000), future, ErrStale},
		{"signed but no timestamp field", fixtureSecret, sign(fixtureSecret, noStamp, fixtureTS), noStamp, ErrMalformed},
		{"signed but no event field", fixtureSecret, sign(fixtureSecret, noEvent, fixtureTS), noEvent, ErrMalformed},
		{"signed but not JSON", fixtureSecret, sign(fixtureSecret, "not json", fixtureTS), "not json", ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify([]byte(tc.secret), tc.header, []byte(tc.body), fixtureNow)
			if !errors.Is(err, tc.want) {
				t.Errorf("Verify error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyAcceptsTheUITestEvent(t *testing.T) {
	body := `{"event":"test","project":{"workspaceId":"p","projectId":"p","projectName":"Home","environment":"prod","environmentName":"Production","secretPath":"/"},"timestamp":1791200000000}`
	ev, err := Verify([]byte(fixtureSecret), sign(fixtureSecret, body, fixtureTS), []byte(body), fixtureNow)
	if err != nil || ev.Type != EventTest {
		t.Errorf("Verify = %+v, %v; want a test event", ev, err)
	}
}
