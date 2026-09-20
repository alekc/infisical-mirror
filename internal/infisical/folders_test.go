package infisical

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListFoldersNonRecursive(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"folders": []map[string]any{
			{"id": "f1", "name": "arr"},
			{"id": "f2", "name": "media"},
		}})
	})

	got, err := newTestClient(t, fake).ListFolders(context.Background(), FolderListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps",
	})
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}

	req := fake.only(t)
	if req.Path != "/api/v2/folders" || req.Query["recursive"] != "false" {
		t.Fatalf("unexpected request: %s %v", req.Path, req.Query)
	}
	if len(got) != 2 || got[0].Path != "/apps/arr" || got[1].Path != "/apps/media" {
		t.Fatalf("paths should be the queried path plus the name, got %+v", got)
	}
}

// relativePath is relative to the queried folder, so a grandchild only lands in
// the right place if that field is used rather than the bare name.
func TestListFoldersRecursiveUsesRelativePath(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"folders": []map[string]any{
			{"id": "f1", "name": "arr", "relativePath": "/arr"},
			{"id": "f2", "name": "config", "relativePath": "/arr/config"},
		}})
	})

	got, err := newTestClient(t, fake).ListFolders(context.Background(), FolderListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps", Recursive: true,
	})
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if fake.only(t).Query["recursive"] != "true" {
		t.Error("recursive was not requested")
	}
	if len(got) != 2 || got[0].Path != "/apps/arr" || got[1].Path != "/apps/arr/config" {
		t.Fatalf("recursive paths wrong: %+v", got)
	}
}

// Without relativePath a nested folder cannot be placed, and guessing would put
// a subtree in the wrong folder, so this fails loudly instead.
func TestListFoldersRecursiveRequiresRelativePath(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"folders": []map[string]any{
			{"id": "f1", "name": "arr"},
		}})
	})

	_, err := newTestClient(t, fake).ListFolders(context.Background(), FolderListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps", Recursive: true,
	})
	if err == nil || !strings.Contains(err.Error(), "relativePath") {
		t.Fatalf("want a relativePath error, got: %v", err)
	}
}

func TestCreateFolderRejectsBadNames(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		t.Error("a bad name should never reach the server")
		writeJSON(t, w, 200, map[string]any{})
	})
	c := newTestClient(t, fake)

	for _, name := range []string{"has space", "slash/inside", "dot.dot", "", "café"} {
		if err := c.CreateFolder(context.Background(), "p", "prod", "/", name); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests were sent for invalid names", n)
	}
}

// EnsureFolder walks parents first, listing each level and creating only what
// is missing.
func TestEnsureFolderCreatesMissingChain(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == "POST" {
			writeJSON(t, w, 200, map[string]any{"folder": map[string]any{"name": "created"}})
			return
		}
		// /apps exists at the root, nothing exists below it.
		folders := []map[string]any{}
		if r.URL.Query().Get("path") == "/" {
			folders = append(folders, map[string]any{"id": "f1", "name": "apps"})
		}
		writeJSON(t, w, 200, map[string]any{"folders": folders})
	})

	if err := newTestClient(t, fake).EnsureFolder(context.Background(), "p", "prod", "/apps/arr/config"); err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}

	var created [][2]string
	for _, req := range fake.seen() {
		if req.Method == "POST" {
			created = append(created, [2]string{req.Body["path"].(string), req.Body["name"].(string)})
		}
	}
	want := [][2]string{{"/apps", "arr"}, {"/apps/arr", "config"}}
	if len(created) != len(want) {
		t.Fatalf("created %v, want %v", created, want)
	}
	for i := range want {
		if created[i] != want[i] {
			t.Errorf("create %d = %v, want %v", i, created[i], want[i])
		}
	}
}

func TestEnsureFolderRootIsNoop(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		t.Error("the root always exists, nothing should be sent")
		writeJSON(t, w, 200, map[string]any{})
	})

	for _, p := range []string{"/", "", "//"} {
		if err := newTestClient(t, fake).EnsureFolder(context.Background(), "p", "prod", p); err != nil {
			t.Fatalf("EnsureFolder(%q): %v", p, err)
		}
	}
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests were sent for the root", n)
	}
}

// Folders seen or created once are not looked up again, which is what keeps a
// reconcile over a deep tree from re-listing every parent per secret.
func TestEnsureFolderCachesWhatItSaw(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == "POST" {
			writeJSON(t, w, 200, map[string]any{})
			return
		}
		writeJSON(t, w, 200, map[string]any{"folders": []map[string]any{
			{"id": "f1", "name": "apps"},
		}})
	})

	c := newTestClient(t, fake)
	ctx := context.Background()
	for range 3 {
		if err := c.EnsureFolder(ctx, "p", "prod", "/apps"); err != nil {
			t.Fatalf("EnsureFolder: %v", err)
		}
	}
	if n := len(fake.seen()); n != 1 {
		t.Fatalf("server saw %d calls, want 1: the folder should be cached after the first listing", n)
	}

	// A different environment is a different folder tree, so it must not be
	// answered from the first one's cache.
	if err := c.EnsureFolder(ctx, "p", "dev", "/apps"); err != nil {
		t.Fatalf("EnsureFolder(dev): %v", err)
	}
	if n := len(fake.seen()); n != 2 {
		t.Fatalf("server saw %d calls, want 2: the cache must be scoped per environment", n)
	}
}

// Something else creating the folder between the listing and the create is the
// outcome we wanted, not a failure.
func TestEnsureFolderToleratesConcurrentCreate(t *testing.T) {
	for name, response := range map[string]struct {
		status int
		body   string
	}{
		"409 conflict":       {409, `{"message":"Folder already exists"}`},
		"400 already exists": {400, `{"message":"Folder with name arr already exists at path /apps"}`},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				if r.Method == "POST" {
					w.WriteHeader(response.status)
					_, _ = io.WriteString(w, response.body)
					return
				}
				writeJSON(t, w, 200, map[string]any{"folders": []any{}})
			})

			if err := newTestClient(t, fake).EnsureFolder(context.Background(), "p", "prod", "/apps"); err != nil {
				t.Fatalf("EnsureFolder should tolerate %s, got: %v", name, err)
			}
		})
	}
}

// Any other create failure must surface: a permission error swallowed here
// becomes a confusing "folder not found" on the write that follows.
func TestEnsureFolderPropagatesRealErrors(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == "POST" {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"message":"forbidden"}`)
			return
		}
		writeJSON(t, w, 200, map[string]any{"folders": []any{}})
	})

	err := newTestClient(t, fake).EnsureFolder(context.Background(), "p", "prod", "/apps")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("want the 403 to surface, got: %v", err)
	}
}

func TestNormalizePath(t *testing.T) {
	tests := map[string]string{
		"":              "/",
		"/":             "/",
		"apps":          "/apps",
		"/apps/":        "/apps",
		"//apps//arr//": "/apps/arr",
		"/apps/./arr":   "/apps/arr",
		"  /apps  ":     "/apps",
	}
	for in, want := range tests {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
