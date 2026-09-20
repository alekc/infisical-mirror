package infisical

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Environment is one environment of a project.
type Environment struct {
	ID   string
	Slug string
	Name string
}

// Project is an Infisical project, resolved from the slug a config names.
type Project struct {
	ID           string
	Slug         string
	Name         string
	Environments []Environment
}

// EnvironmentSlugs returns the environment slugs, sorted, for error messages.
func (p *Project) EnvironmentSlugs() []string {
	slugs := make([]string, 0, len(p.Environments))
	for _, env := range p.Environments {
		slugs = append(slugs, env.Slug)
	}
	sort.Strings(slugs)
	return slugs
}

// HasEnvironment reports whether the project has an environment with this slug.
func (p *Project) HasEnvironment(slug string) bool {
	return slices.ContainsFunc(p.Environments, func(env Environment) bool { return env.Slug == slug })
}

// apiProject is the project as the API returns it.
type apiProject struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Environments []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"environments"`
}

// ProjectBySlug resolves a project slug, the identifier in the Infisical URL,
// to the project itself. The secret and folder endpoints take an ID, so every
// run starts here. The result is cached for the life of the client: an ID does
// not change, and a re-point in the config is a new run.
func (c *Client) ProjectBySlug(ctx context.Context, slug string) (*Project, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("infisical: project slug is empty")
	}

	if cached := c.projects.get(slug); cached != nil {
		return cached, nil
	}

	// Both live instances, cloud and self-hosted, return the project at the
	// top level. The wrapped form is accepted as well because the envelope is
	// not something this endpoint promises, and an unwrap that silently
	// yields a zero-valued project reads as "no such project".
	var resp struct {
		apiProject
		Project *apiProject `json:"project"`
	}
	if err := c.do(ctx, "GET", "/api/v1/projects/slug/"+url.PathEscape(slug), nil, nil, &resp); err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("infisical: no project with slug %q (the config names the slug from the project URL, not the project id): %w", slug, err)
		}
		return nil, err
	}

	found := resp.apiProject
	if found.ID == "" && resp.Project != nil {
		found = *resp.Project
	}
	if found.ID == "" {
		return nil, fmt.Errorf("infisical: project %q resolved to no id", slug)
	}

	project := &Project{ID: found.ID, Slug: found.Slug, Name: found.Name}
	for _, env := range found.Environments {
		project.Environments = append(project.Environments, Environment{ID: env.ID, Slug: env.Slug, Name: env.Name})
	}

	c.projects.put(slug, project)
	return project, nil
}

// ResolveProjectID returns the project ID for a slug, having first checked that
// every environment the caller intends to touch exists. Checking here rather
// than at the first read keeps a typo in an environment name from looking like
// an empty folder, which a sync reads as "that whole side was deleted".
func (c *Client) ResolveProjectID(ctx context.Context, slug string, environments ...string) (string, error) {
	project, err := c.ProjectBySlug(ctx, slug)
	if err != nil {
		return "", err
	}

	var missing []string
	for _, env := range environments {
		if !project.HasEnvironment(env) {
			missing = append(missing, env)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("infisical: project %q has no environment(s) %s; it has %s",
			slug, strings.Join(missing, ", "), strings.Join(project.EnvironmentSlugs(), ", "))
	}
	return project.ID, nil
}

type projectCache struct {
	mu     sync.Mutex
	bySlug map[string]*Project
}

func (p *projectCache) get(slug string) *Project {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bySlug[slug]
}

func (p *projectCache) put(slug string, project *Project) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bySlug == nil {
		p.bySlug = map[string]*Project{}
	}
	p.bySlug[slug] = project
}
