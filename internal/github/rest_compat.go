package github

// REST-only implementations of the pull request and repository lookups that
// upstream gh-stack performs over GraphQL. Some environments (for example
// Claude Code cloud sessions) route GitHub traffic through a proxy that rejects
// GraphQL outright, so every call here sticks to REST.
//
// Two operations have no public REST equivalent and use routes served by the
// Claude Code GitHub proxy instead:
//
//	POST   /repos/{owner}/{repo}/pulls/{n}/ccr/ready_for_review
//	DELETE /repos/{owner}/{repo}/pulls/{n}/ccr/auto_merge
//
// Known gaps compared to GraphQL:
//   - REST does not expose a PR's merge queue entry, so MergeQueueEntry is
//     always nil and IsQueued always reports false.
//   - REST does not expose the viewer's last-used merge method, so
//     RepoMergeConfig.DefaultMethod is the first allowed of merge, squash,
//     rebase.
//   - Branch lookups match PRs whose head is in the same repository
//     (head=owner:branch), not PRs opened from forks.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// restPullRequest is the subset of the REST pull request payload gh-stack uses.
type restPullRequest struct {
	NodeID    string  `json:"node_id"`
	Number    int     `json:"number"`
	State     string  `json:"state"`
	HTMLURL   string  `json:"html_url"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	Draft     bool    `json:"draft"`
	MergedAt  *string `json:"merged_at"`
	AutoMerge *struct {
		EnabledBy *struct {
			Login string `json:"login"`
		} `json:"enabled_by"`
	} `json:"auto_merge"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// graphQLState maps a REST state onto the GraphQL PullRequestState enum
// (OPEN, CLOSED, MERGED) that the rest of gh-stack expects.
func (p *restPullRequest) graphQLState() string {
	if p.MergedAt != nil {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
}

func (p *restPullRequest) toPullRequest() *PullRequest {
	pr := &PullRequest{
		ID:          p.NodeID,
		Number:      p.Number,
		State:       p.graphQLState(),
		URL:         p.HTMLURL,
		Title:       p.Title,
		Body:        p.Body,
		HeadRefName: p.Head.Ref,
		BaseRefName: p.Base.Ref,
		IsDraft:     p.Draft,
		Merged:      p.MergedAt != nil,
	}
	if p.AutoMerge != nil {
		// REST does not report when auto-merge was enabled; any non-empty
		// value works because callers only check for presence.
		pr.AutoMergeRequest = &AutoMergeRequest{EnabledAt: "unknown"}
	}
	return pr
}

// rememberPR records the node ID -> number mapping for a fetched PR, so that
// methods taking a node ID can address the PR by number over REST.
func (c *Client) rememberPR(p *restPullRequest) {
	if p == nil || p.NodeID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prNumbers == nil {
		c.prNumbers = make(map[string]int)
	}
	c.prNumbers[p.NodeID] = p.Number
}

func (c *Client) prNumberForID(prID string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.prNumbers[prID]
	if !ok {
		return 0, fmt.Errorf("unknown pull request ID %q: fetch the PR before acting on it", prID)
	}
	return n, nil
}

func isNotFound(err error) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

// listPRsForBranch lists PRs whose head is branch in this repository.
func (c *Client) listPRsForBranch(branch, state string, limit int) ([]restPullRequest, error) {
	q := url.Values{}
	q.Set("head", c.owner+":"+branch)
	q.Set("state", state)
	q.Set("sort", "created")
	q.Set("direction", "desc")
	q.Set("per_page", fmt.Sprint(limit))
	path := fmt.Sprintf("repos/%s/%s/pulls?%s", c.owner, c.repo, q.Encode())

	var prs []restPullRequest
	if err := c.rest.Get(path, &prs); err != nil {
		return nil, err
	}
	for i := range prs {
		c.rememberPR(&prs[i])
	}
	return prs, nil
}

// FindPRForBranch finds an open PR by head branch name.
func (c *Client) FindPRForBranch(branch string) (*PullRequest, error) {
	prs, err := c.listPRsForBranch(branch, "open", 1)
	if err != nil {
		return nil, fmt.Errorf("querying PRs: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return prs[0].toPullRequest(), nil
}

// FindPRDetailsForBranch fetches enriched PR data for display purposes.
// Returns nil without error if no PR exists for the branch.
func (c *Client) FindPRDetailsForBranch(branch string) (*PRDetails, error) {
	prs, err := c.listPRsForBranch(branch, "all", 1)
	if err != nil {
		return nil, fmt.Errorf("querying PR details: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	p := prs[0]
	return &PRDetails{
		Number:  p.Number,
		State:   p.graphQLState(),
		URL:     p.HTMLURL,
		Title:   p.Title,
		Body:    p.Body,
		IsDraft: p.Draft,
		Merged:  p.MergedAt != nil,
	}, nil
}

// FindPRByNumber fetches a pull request by its number. Returns nil without
// error if the PR does not exist.
func (c *Client) FindPRByNumber(number int) (*PullRequest, error) {
	var p restPullRequest
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", c.owner, c.repo, number)
	if err := c.rest.Get(path, &p); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying PR #%d: %w", number, err)
	}
	c.rememberPR(&p)
	return p.toPullRequest(), nil
}

// CreatePR creates a new pull request.
func (c *Client) CreatePR(base, head, title, body string, draft bool) (*PullRequest, error) {
	type createPRRequest struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body,omitempty"`
		Draft bool   `json:"draft"`
	}

	payload, err := json.Marshal(createPRRequest{Title: title, Head: head, Base: base, Body: body, Draft: draft})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	var p restPullRequest
	path := fmt.Sprintf("repos/%s/%s/pulls", c.owner, c.repo)
	if err := c.rest.Post(path, bytes.NewReader(payload), &p); err != nil {
		return nil, fmt.Errorf("creating PR: %w", err)
	}
	c.rememberPR(&p)
	return &PullRequest{
		ID:     p.NodeID,
		Number: p.Number,
		URL:    p.HTMLURL,
	}, nil
}

// MarkPRReadyForReview converts a draft pull request to ready for review.
func (c *Client) MarkPRReadyForReview(prID string) error {
	number, err := c.prNumberForID(prID)
	if err != nil {
		return fmt.Errorf("marking PR ready for review: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/ccr/ready_for_review", c.owner, c.repo, number)
	if err := c.rest.Post(path, nil, nil); err != nil {
		return fmt.Errorf("marking PR ready for review: %w", err)
	}
	return nil
}

// DisableAutoMerge disables auto-merge on a pull request.
func (c *Client) DisableAutoMerge(prID string) error {
	number, err := c.prNumberForID(prID)
	if err != nil {
		return fmt.Errorf("disabling auto-merge: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/ccr/auto_merge", c.owner, c.repo, number)
	if err := c.rest.Delete(path, nil); err != nil {
		return fmt.Errorf("disabling auto-merge: %w", err)
	}
	return nil
}

// RepoMergeConfig fetches the repository's allowed merge methods. REST does
// not expose the viewer's last-used method, so DefaultMethod is the first
// allowed of merge, squash, rebase.
func (c *Client) RepoMergeConfig() (*RepoMergeConfig, error) {
	var repo struct {
		AllowMergeCommit bool `json:"allow_merge_commit"`
		AllowSquashMerge bool `json:"allow_squash_merge"`
		AllowRebaseMerge bool `json:"allow_rebase_merge"`
	}
	if err := c.rest.Get(fmt.Sprintf("repos/%s/%s", c.owner, c.repo), &repo); err != nil {
		return nil, fmt.Errorf("querying repository merge config: %w", err)
	}

	cfg := &RepoMergeConfig{
		MergeAllowed:  repo.AllowMergeCommit,
		SquashAllowed: repo.AllowSquashMerge,
		RebaseAllowed: repo.AllowRebaseMerge,
		DefaultMethod: MergeMethodMerge,
	}
	switch {
	case cfg.MergeAllowed:
		cfg.DefaultMethod = MergeMethodMerge
	case cfg.SquashAllowed:
		cfg.DefaultMethod = MergeMethodSquash
	case cfg.RebaseAllowed:
		cfg.DefaultMethod = MergeMethodRebase
	}
	return cfg, nil
}

// BaseBranchUsesMergeQueue reports whether the given base branch merges through
// a merge queue, detected via a merge_queue rule among the rules that apply to
// the branch. It is used only to tailor the merge wizard.
func (c *Client) BaseBranchUsesMergeQueue(baseRef string) (bool, error) {
	var rules []struct {
		Type string `json:"type"`
	}
	path := fmt.Sprintf("repos/%s/%s/rules/branches/%s", c.owner, c.repo, baseRef)
	if err := c.rest.Get(path, &rules); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("querying base branch merge queue: %w", err)
	}
	for _, r := range rules {
		if r.Type == "merge_queue" {
			return true, nil
		}
	}
	return false, nil
}

// PRTitles fetches the titles for a set of pull request numbers, one REST call
// per PR. Missing PRs are simply absent from the result. Best-effort: callers
// may ignore the error and proceed without titles.
func (c *Client) PRTitles(numbers []int) (map[int]string, error) {
	titles := make(map[int]string, len(numbers))
	for _, n := range numbers {
		var p restPullRequest
		path := fmt.Sprintf("repos/%s/%s/pulls/%d", c.owner, c.repo, n)
		if err := c.rest.Get(path, &p); err != nil {
			if isNotFound(err) {
				continue
			}
			return titles, fmt.Errorf("querying pull request titles: %w", err)
		}
		c.rememberPR(&p)
		if p.Number != 0 {
			titles[p.Number] = p.Title
		}
	}
	return titles, nil
}
