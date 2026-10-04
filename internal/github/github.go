package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"

	"github.com/cli/go-gh/v2/pkg/api"
	graphql "github.com/cli/shurcooL-graphql"
)

// MergeQueueEntry represents a merge queue entry. When the GraphQL field
// mergeQueueEntry is null (PR not queued), the pointer will be nil.
type MergeQueueEntry struct {
	ID string `graphql:"id"`
}

// AutoMergeRequest represents an auto-merge configuration on a PR.
// When the GraphQL field autoMergeRequest is null (auto-merge not enabled),
// the pointer will be nil.
type AutoMergeRequest struct {
	EnabledAt string `graphql:"enabledAt"`
}

// PullRequest represents a GitHub pull request.
type PullRequest struct {
	ID               string            `graphql:"id"`
	Number           int               `graphql:"number"`
	State            string            `graphql:"state"`
	URL              string            `graphql:"url"`
	Title            string            `graphql:"title"`
	Body             string            `graphql:"body"`
	HeadRefName      string            `graphql:"headRefName"`
	BaseRefName      string            `graphql:"baseRefName"`
	IsDraft          bool              `graphql:"isDraft"`
	Merged           bool              `graphql:"merged"`
	MergeQueueEntry  *MergeQueueEntry  `graphql:"mergeQueueEntry"`
	AutoMergeRequest *AutoMergeRequest `graphql:"autoMergeRequest"`
}

// IsQueued reports whether the pull request is currently in a merge queue.
func (pr *PullRequest) IsQueued() bool {
	return pr != nil && pr.MergeQueueEntry != nil && pr.MergeQueueEntry.ID != ""
}

// IsAutoMergeEnabled reports whether the pull request has auto-merge enabled.
func (pr *PullRequest) IsAutoMergeEnabled() bool {
	return pr != nil && pr.AutoMergeRequest != nil
}

// Client wraps GitHub API operations.
type Client struct {
	rest  *api.RESTClient
	host  string
	owner string
	repo  string
	slug  string

	// mu guards prNumbers, which maps PR node IDs seen in responses to PR
	// numbers so node-ID based methods can address PRs over REST.
	mu        sync.Mutex
	prNumbers map[string]int
}

// NewClient creates a new GitHub API client for the given repository.
// The host parameter specifies the GitHub hostname (e.g. "github.com" or a
// GHES hostname like "github.mycompany.com"). If empty, it defaults to
// "github.com".
func NewClient(host, owner, repo string) (*Client, error) {
	if host == "" {
		host = "github.com"
	}
	opts := api.ClientOptions{Host: host}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, fmt.Errorf("creating REST client: %w", err)
	}
	return &Client{
		rest:  rest,
		host:  host,
		owner: owner,
		repo:  repo,
		slug:  owner + "/" + repo,
	}, nil
}

// PRURL constructs the web URL for a pull request on the given host.
func PRURL(host, owner, repo string, number int) string {
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://%s/%s/%s/pull/%d", host, owner, repo, number)
}

// UpdatePRBase updates the base branch of an existing pull request.
func (c *Client) UpdatePRBase(number int, base string) error {
	type updatePRRequest struct {
		Base string `json:"base"`
	}

	body, err := json.Marshal(updatePRRequest{Base: base})
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	path := fmt.Sprintf("repos/%s/%s/pulls/%d", c.owner, c.repo, number)
	return c.rest.Patch(path, bytes.NewReader(body), nil)
}

// PRDetails holds enriched pull request data for display in the TUI.
type PRDetails struct {
	Number   int
	State    string // OPEN, CLOSED, MERGED
	URL      string
	Title    string
	Body     string
	IsDraft  bool
	Merged   bool
	IsQueued bool
}

func toGraphQLInt(n int) (graphql.Int, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("number %d is out of GraphQL Int range", n)
	}
	return graphql.Int(n), nil
}

// RemoteStackBase describes the base ref (and optionally SHA) of a stack.
type RemoteStackBase struct {
	Ref string `json:"ref"`
	Sha string `json:"sha,omitempty"`
}

// RemoteStackPRHead describes the head ref of a pull request in a stack.
type RemoteStackPRHead struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

// RemoteStackPR is a pull request entry within a remote stack, as returned by
// the Stacks REST API list/detail endpoints.
type RemoteStackPR struct {
	Number   int               `json:"number"`
	State    string            `json:"state"` // open, closed
	Draft    bool              `json:"draft"`
	MergedAt *string           `json:"merged_at"`
	Head     RemoteStackPRHead `json:"head"`
}

// IsMerged reports whether the pull request has been merged.
func (p RemoteStackPR) IsMerged() bool {
	return p.MergedAt != nil && *p.MergedAt != ""
}

// RemoteStack represents a stack of pull requests as returned by the public
// Stacks REST API (GET/POST /repos/{owner}/{repo}/stacks...). ID is the
// internal identifier; Number is the human-facing stack number shown in the
// github.com UI and used to address the stack in API paths.
//
// The API returns pull_requests as an array of objects; UnmarshalJSON flattens
// them to the ordered PullRequests numbers (bottom to top) and preserves the
// full entries in PRDetails for callers that need head refs or PR state.
type RemoteStack struct {
	ID           int             `json:"id"`
	Number       int             `json:"number"`
	NodeID       string          `json:"node_id"`
	URL          string          `json:"url"`
	Base         RemoteStackBase `json:"base"`
	Open         bool            `json:"open"`
	CreatedAt    string          `json:"created_at"`
	PullRequests []int           `json:"-"`
	PRDetails    []RemoteStackPR `json:"-"`
}

// UnmarshalJSON decodes the Stacks REST API representation, deriving the
// ordered PullRequests numbers from the pull_requests objects.
func (s *RemoteStack) UnmarshalJSON(data []byte) error {
	type wire struct {
		ID           int             `json:"id"`
		Number       int             `json:"number"`
		NodeID       string          `json:"node_id"`
		URL          string          `json:"url"`
		Base         RemoteStackBase `json:"base"`
		Open         bool            `json:"open"`
		CreatedAt    string          `json:"created_at"`
		PullRequests []RemoteStackPR `json:"pull_requests"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	s.ID = w.ID
	s.Number = w.Number
	s.NodeID = w.NodeID
	s.URL = w.URL
	s.Base = w.Base
	s.Open = w.Open
	s.CreatedAt = w.CreatedAt
	s.PRDetails = w.PullRequests
	s.PullRequests = make([]int, len(w.PullRequests))
	for i, p := range w.PullRequests {
		s.PullRequests[i] = p.Number
	}
	return nil
}

// PRNumbers returns the ordered pull request numbers in the stack, from bottom
// to top.
func (s *RemoteStack) PRNumbers() []int {
	return s.PullRequests
}

// ListStacks returns all stacks in the repository, ordered by stack number
// (descending). Returns an empty slice if no stacks exist. A 404 response
// indicates stacked PRs are not enabled for this repository.
func (c *Client) ListStacks() ([]RemoteStack, error) {
	const perPage = 100

	var stacks []RemoteStack
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/stacks?per_page=%d&page=%d", c.owner, c.repo, perPage, page)
		var batch []RemoteStack
		if err := c.rest.Get(path, &batch); err != nil {
			return nil, err
		}
		stacks = append(stacks, batch...)
		if len(batch) < perPage {
			break
		}
	}
	if stacks == nil {
		stacks = []RemoteStack{}
	}
	return stacks, nil
}

// FindStackForPR returns the stack that contains the given pull request number,
// using the list endpoint's server-side pull_request filter. Returns nil
// (without error) when the PR is not part of any stack.
func (c *Client) FindStackForPR(prNumber int) (*RemoteStack, error) {
	path := fmt.Sprintf("repos/%s/%s/stacks?pull_request=%d", c.owner, c.repo, prNumber)
	var stacks []RemoteStack
	if err := c.rest.Get(path, &stacks); err != nil {
		return nil, err
	}
	if len(stacks) == 0 {
		return nil, nil
	}
	return &stacks[0], nil
}

// GetStack fetches a single stack by its stack number.
func (c *Client) GetStack(stackNumber int) (*RemoteStack, error) {
	path := fmt.Sprintf("repos/%s/%s/stacks/%d", c.owner, c.repo, stackNumber)
	var rs RemoteStack
	if err := c.rest.Get(path, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// CreateStack creates a stack on GitHub from an ordered list of PR numbers.
// The PR numbers must be ordered from bottom to top of the stack (at least two)
// and must form a valid base-to-head chain. Returns the created stack.
func (c *Client) CreateStack(prNumbers []int) (*RemoteStack, error) {
	type createStackRequest struct {
		PullRequests []int `json:"pull_requests"`
	}

	body, err := json.Marshal(createStackRequest{PullRequests: prNumbers})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	path := fmt.Sprintf("repos/%s/%s/stacks", c.owner, c.repo)
	var rs RemoteStack
	if err := c.rest.Post(path, bytes.NewReader(body), &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// AddToStack appends pull requests to the top of an existing stack. Only the
// new PR numbers (the delta) should be provided, ordered from the current top
// of the stack upward. Returns the updated stack.
func (c *Client) AddToStack(stackNumber int, prNumbers []int) (*RemoteStack, error) {
	type addToStackRequest struct {
		PullRequests []int `json:"pull_requests"`
	}

	body, err := json.Marshal(addToStackRequest{PullRequests: prNumbers})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	path := fmt.Sprintf("repos/%s/%s/stacks/%d/add", c.owner, c.repo, stackNumber)
	var rs RemoteStack
	if err := c.rest.Post(path, bytes.NewReader(body), &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// Unstack removes unlocked pull requests from a stack. The server leaves PRs
// that cannot be unstacked (queued for merge or with auto-merge enabled) in
// place. When PRs remain, the updated stack is returned with dissolved=false;
// when none remain the stack is destroyed and dissolved=true (HTTP 204).
func (c *Client) Unstack(stackNumber int) (rs *RemoteStack, dissolved bool, err error) {
	path := fmt.Sprintf("repos/%s/%s/stacks/%d/unstack", c.owner, c.repo, stackNumber)
	resp, err := c.rest.Request(http.MethodPost, path, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		return nil, true, nil
	}

	var remaining RemoteStack
	if decErr := json.NewDecoder(resp.Body).Decode(&remaining); decErr != nil {
		return nil, false, fmt.Errorf("decoding unstack response: %w", decErr)
	}
	return &remaining, false, nil
}
