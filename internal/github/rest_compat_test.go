package github

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const restPRJSON = `{"node_id":"PR_abc","number":7,"state":"open","html_url":"https://github.com/o/r/pull/7",
"title":"T","body":"B","draft":true,"merged_at":null,"auto_merge":{"enabled_by":{"login":"me"}},
"head":{"ref":"feat"},"base":{"ref":"main"}}`

func TestFindPRForBranch_REST(t *testing.T) {
	var path, rawQuery string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path, rawQuery = r.URL.Path, r.URL.RawQuery
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("[" + restPRJSON + "]")),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    r,
		}, nil
	})
	rest, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
	require.NoError(t, err)
	c := &Client{rest: rest, owner: "o", repo: "r"}

	pr, err := c.FindPRForBranch("feat")
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "/repos/o/r/pulls", path)
	assert.Contains(t, rawQuery, "head=o%3Afeat")
	assert.Contains(t, rawQuery, "state=open")
	assert.Equal(t, "PR_abc", pr.ID)
	assert.Equal(t, 7, pr.Number)
	assert.Equal(t, "OPEN", pr.State)
	assert.Equal(t, "feat", pr.HeadRefName)
	assert.Equal(t, "main", pr.BaseRefName)
	assert.True(t, pr.IsDraft)
	assert.True(t, pr.IsAutoMergeEnabled())
	assert.False(t, pr.IsQueued())
}

func TestFindPRForBranch_NoneOpen(t *testing.T) {
	c := testAsyncClient(t, http.StatusOK, "[]", nil)
	pr, err := c.FindPRForBranch("feat")
	require.NoError(t, err)
	assert.Nil(t, pr)
}

func TestFindPRByNumber_NotFound(t *testing.T) {
	c := testAsyncClient(t, http.StatusNotFound, `{"message":"Not Found"}`, nil)
	pr, err := c.FindPRByNumber(9)
	require.NoError(t, err)
	assert.Nil(t, pr)
}

func TestFindPRDetailsForBranch_Merged(t *testing.T) {
	c := testAsyncClient(t, http.StatusOK,
		`[{"node_id":"PR_x","number":3,"state":"closed","merged_at":"2026-01-01T00:00:00Z"}]`, nil)
	d, err := c.FindPRDetailsForBranch("feat")
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, "MERGED", d.State)
	assert.True(t, d.Merged)
}

func TestCreatePR_REST(t *testing.T) {
	rec := &recordedRequest{}
	c := testAsyncClient(t, http.StatusCreated, restPRJSON, rec)
	pr, err := c.CreatePR("main", "feat", "T", "B", true)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/repos/o/r/pulls", rec.path)
	assert.JSONEq(t, `{"title":"T","head":"feat","base":"main","body":"B","draft":true}`, rec.body)
	assert.Equal(t, "PR_abc", pr.ID)
	assert.Equal(t, 7, pr.Number)
}

func TestNodeIDMethods_UseCCRRoutes(t *testing.T) {
	rec := &recordedRequest{}
	c := testAsyncClient(t, http.StatusOK, restPRJSON, rec)
	_, err := c.FindPRByNumber(7)
	require.NoError(t, err)

	require.NoError(t, c.MarkPRReadyForReview("PR_abc"))
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/repos/o/r/pulls/7/ccr/ready_for_review", rec.path)

	require.NoError(t, c.DisableAutoMerge("PR_abc"))
	assert.Equal(t, http.MethodDelete, rec.method)
	assert.Equal(t, "/repos/o/r/pulls/7/ccr/auto_merge", rec.path)
}

func TestNodeIDMethods_UnknownID(t *testing.T) {
	c := testAsyncClient(t, http.StatusOK, "{}", nil)
	assert.ErrorContains(t, c.MarkPRReadyForReview("PR_missing"), "unknown pull request ID")
	assert.ErrorContains(t, c.DisableAutoMerge("PR_missing"), "unknown pull request ID")
}

func TestRepoMergeConfig_REST(t *testing.T) {
	c := testAsyncClient(t, http.StatusOK,
		`{"allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":true}`, nil)
	cfg, err := c.RepoMergeConfig()
	require.NoError(t, err)
	assert.Equal(t, []string{MergeMethodSquash, MergeMethodRebase}, cfg.AllowedMethods())
	assert.Equal(t, MergeMethodSquash, cfg.DefaultMethod)
}

func TestBaseBranchUsesMergeQueue_REST(t *testing.T) {
	c := testAsyncClient(t, http.StatusOK, `[{"type":"pull_request"},{"type":"merge_queue"}]`, nil)
	queued, err := c.BaseBranchUsesMergeQueue("main")
	require.NoError(t, err)
	assert.True(t, queued)

	c = testAsyncClient(t, http.StatusOK, `[]`, nil)
	queued, err = c.BaseBranchUsesMergeQueue("main")
	require.NoError(t, err)
	assert.False(t, queued)
}

func TestPRTitles_SkipsMissing(t *testing.T) {
	c := testAsyncClient(t, http.StatusNotFound, `{"message":"Not Found"}`, nil)
	titles, err := c.PRTitles([]int{1, 2})
	require.NoError(t, err)
	assert.Empty(t, titles)
}
