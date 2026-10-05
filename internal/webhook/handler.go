package webhook

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// MaxBody caps a request body. A real payload is a few hundred bytes.
const MaxBody = 64 << 10

// Outcome labels for a request, as counted by the metrics.
const (
	OutcomeAccepted      = "accepted"
	OutcomeTest          = "test"
	OutcomeIgnored       = "ignored"
	OutcomeUnsigned      = "unsigned"
	OutcomeBadSignature  = "bad_signature"
	OutcomeStale         = "stale"
	OutcomeMalformed     = "malformed"
	OutcomeTooLarge      = "too_large"
	OutcomeUnknownSource = "unknown_source"
)

// Handler serves POST /webhook/{instance}. Secrets maps an instance name to
// its webhook secret; Trigger is called once per accepted event and must not
// block; Observe, when set, is told every request's source and outcome.
type Handler struct {
	Secrets map[string][]byte
	Trigger func()
	Observe func(source, outcome string)
	Now     func() time.Time
}

// Mux is the receiver's whole HTTP surface: one route, nothing else, because
// this is the port that gets exposed. Other methods get 405, other paths 404.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /webhook/{instance}", h)
	return mux
}

// ServeHTTP answers with a bare status and no body, so a caller without the
// secret learns nothing beyond the code.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("instance")
	secret, ok := h.Secrets[source]
	if !ok {
		// The path is caller-chosen, so it never becomes a label value.
		h.observe("", OutcomeUnknownSource)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			h.observe(source, OutcomeTooLarge)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.observe(source, OutcomeMalformed)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	ev, err := Verify(secret, r.Header.Get("x-infisical-signature"), body, now())
	switch {
	case errors.Is(err, ErrUnsigned):
		h.reject(w, source, OutcomeUnsigned)
		return
	case errors.Is(err, ErrBadSignature):
		h.reject(w, source, OutcomeBadSignature)
		return
	case errors.Is(err, ErrStale):
		h.reject(w, source, OutcomeStale)
		return
	case err != nil:
		h.reject(w, source, OutcomeMalformed)
		return
	}

	switch ev.Type {
	case EventSecretsModified:
		h.Trigger()
		h.observe(source, OutcomeAccepted)
		w.WriteHeader(http.StatusAccepted)
	case EventTest:
		// The UI's Test button: proves the URL and secret, starts nothing.
		h.observe(source, OutcomeTest)
		w.WriteHeader(http.StatusOK)
	default:
		h.observe(source, OutcomeIgnored)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) reject(w http.ResponseWriter, source, outcome string) {
	h.observe(source, outcome)
	w.WriteHeader(http.StatusUnauthorized)
}

func (h *Handler) observe(source, outcome string) {
	if h.Observe != nil {
		h.Observe(source, outcome)
	}
}
