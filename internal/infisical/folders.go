package infisical

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Infisical validates folder names as alphanumerics, dashes and underscores.
// Checked here so a bad name fails before a write rather than as a 400 halfway
// through creating a chain of folders.
var folderNamePattern = regexp.MustCompile(`^[a-zA-Z0-9-_]+$`)

// Folder is one folder on an instance.
type Folder struct {
	ID   string
	Name string
	// Path is absolute on the instance, leading slash, no trailing slash.
	Path string
}

// FolderListRequest asks for the folders under Path.
type FolderListRequest struct {
	ProjectID   string
	Environment string
	Path        string
	// Recursive returns the whole subtree rather than the direct children.
	Recursive bool
}

type apiFolder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// RelativePath is set only on a recursive listing, and is relative to the
	// queried folder with a leading slash: the server seeds the walk at the
	// queried folder with path "/" and drops it from the result.
	RelativePath string `json:"relativePath"`
}

// ListFolders returns folders under req.Path with absolute paths.
//
// A recursive listing returns the whole subtree and ignores paging, so there is
// no continuation to follow.
func (c *Client) ListFolders(ctx context.Context, req FolderListRequest) ([]Folder, error) {
	query := url.Values{
		"projectId":   {req.ProjectID},
		"environment": {req.Environment},
		"path":        {normalizePath(req.Path)},
		"recursive":   {strconv.FormatBool(req.Recursive)},
	}

	var resp struct {
		Folders []apiFolder `json:"folders"`
	}
	if err := c.do(ctx, "GET", "/api/v2/folders", query, nil, &resp); err != nil {
		return nil, err
	}

	base := normalizePath(req.Path)
	folders := make([]Folder, 0, len(resp.Folders))
	for _, f := range resp.Folders {
		abs := joinPath(base, f.Name)
		if req.Recursive {
			// Deeper than one level, the name alone cannot place the folder, so
			// a server that does not send relativePath is an error rather than
			// something to guess around.
			if f.RelativePath == "" {
				return nil, fmt.Errorf("infisical: recursive folder listing of %s returned folder %q with no relativePath", base, f.Name)
			}
			abs = joinPath(base, f.RelativePath)
		}
		folders = append(folders, Folder{ID: f.ID, Name: f.Name, Path: abs})
	}
	return folders, nil
}

// CreateFolder creates one folder named name directly under parent.
func (c *Client) CreateFolder(ctx context.Context, projectID, environment, parent, name string) error {
	if !folderNamePattern.MatchString(name) {
		return fmt.Errorf("infisical: folder name %q must contain only letters, digits, dashes and underscores", name)
	}

	body := map[string]string{
		"projectId":   projectID,
		"environment": environment,
		"path":        normalizePath(parent),
		"name":        name,
	}
	return c.do(ctx, "POST", "/api/v2/folders", nil, body, nil)
}

// EnsureFolder creates every missing folder along absPath, parents first.
// Infisical does not create a destination folder implicitly: writing into one
// that does not exist fails with "Folder with path ... not found", so the
// mirror builds the tree itself.
func (c *Client) EnsureFolder(ctx context.Context, projectID, environment, absPath string) error {
	target := normalizePath(absPath)
	if target == "/" {
		return nil
	}

	current := "/"
	for _, name := range strings.Split(strings.Trim(target, "/"), "/") {
		next := joinPath(current, name)
		if c.folderKnown(projectID, environment, next) {
			current = next
			continue
		}

		children, err := c.ListFolders(ctx, FolderListRequest{
			ProjectID:   projectID,
			Environment: environment,
			Path:        current,
		})
		if err != nil {
			return err
		}

		found := false
		for _, child := range children {
			c.markFolderKnown(projectID, environment, child.Path)
			if child.Name == name {
				found = true
			}
		}

		if !found {
			// A parallel run, or a person in the UI, may have created it
			// between the listing and here. That is the outcome we wanted.
			if err := c.CreateFolder(ctx, projectID, environment, current, name); err != nil && !alreadyExists(err) {
				return err
			}
			c.markFolderKnown(projectID, environment, next)
		}
		current = next
	}
	return nil
}

// alreadyExists reports whether err says the folder is already there. The
// server answers a duplicate create with 409 on some paths and a 400 carrying
// its own wording on others, so both are matched.
func alreadyExists(err error) bool {
	if IsConflict(err) {
		return true
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == 400 && strings.Contains(strings.ToLower(apiErr.Message), "already exist")
}

// folderCache remembers folders seen or created during this process. Only
// existence is cached, never absence, so a stale entry cannot cause a write
// into a folder that is not there: the write itself would fail.
type folderCache struct {
	mu    sync.Mutex
	known map[string]bool
}

func (c *Client) folderKnown(projectID, environment, absPath string) bool {
	c.folders.mu.Lock()
	defer c.folders.mu.Unlock()
	return c.folders.known[folderKey(projectID, environment, absPath)]
}

func (c *Client) markFolderKnown(projectID, environment, absPath string) {
	c.folders.mu.Lock()
	defer c.folders.mu.Unlock()
	if c.folders.known == nil {
		c.folders.known = map[string]bool{}
	}
	c.folders.known[folderKey(projectID, environment, absPath)] = true
}

func folderKey(projectID, environment, absPath string) string {
	return projectID + "|" + environment + "|" + absPath
}

// normalizePath canonicalises an instance path: leading slash, no trailing
// slash, "/" for the root.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func joinPath(base, rel string) string {
	return normalizePath(path.Join(normalizePath(base), rel))
}
