package bitbucketserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/runatlantis/atlantis/server/events/vcs/common"
	"github.com/runatlantis/atlantis/server/logging"

	validator "github.com/go-playground/validator/v10"
	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/models"
)

// maxCommentLength is the maximum number of chars allowed by Bitbucket in a
// single comment.
const maxCommentLength = 32768

type Client struct {
	HTTPClient  *http.Client
	Username    string
	Password    string
	BaseURL     string
	AtlantisURL string
}

type DeleteSourceBranch struct {
	Name   string `json:"name"`
	DryRun bool   `json:"dryRun"`
}

// NewClient builds a bitbucket cloud client. Returns an error if the baseURL is
// malformed. httpClient is the client to use to make the requests, username
// and password are used as basic auth in the requests, baseURL is the API's
// baseURL, ex. https://corp.com:7990. Don't include the API version, ex.
// '/1.0' since that changes based on the API call. atlantisURL is the
// URL for Atlantis that will be linked to from the build status icons. This
// linking is annoying because we don't have anywhere good to link but a URL is
// required.
func NewClient(httpClient *http.Client, username string, password string, baseURL string, atlantisURL string) (*Client, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing %s", baseURL)
	}
	if parsedURL.Scheme == "" {
		return nil, fmt.Errorf("must have 'http://' or 'https://' in base url %q", baseURL)
	}
	return &Client{
		HTTPClient:  httpClient,
		Username:    username,
		Password:    password,
		BaseURL:     strings.TrimRight(parsedURL.String(), "/"),
		AtlantisURL: atlantisURL,
	}, nil
}

// GetModifiedFiles returns the names of files that were modified in the merge request
// relative to the repo root, e.g. parent/child/file.txt.
func (b *Client) GetModifiedFiles(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) ([]string, error) {
	var files []string

	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return nil, err
	}
	nextPageStart := 0
	baseURL := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/changes",
		b.BaseURL, projectKey, repo.Name, pull.Num)
	// We'll only loop 1000 times as a safety measure.
	maxLoops := 1000
	for i := 0; i < maxLoops; i++ {
		resp, err := b.makeRequest("GET", fmt.Sprintf("%s?start=%d", baseURL, nextPageStart), nil)
		if err != nil {
			return nil, err
		}
		var changes Changes
		if err := json.Unmarshal(resp, &changes); err != nil {
			return nil, errors.Wrapf(err, "Could not parse response %q", string(resp))
		}
		if err := validator.New().Struct(changes); err != nil {
			return nil, errors.Wrapf(err, "API response %q was missing fields", string(resp))
		}
		for _, v := range changes.Values {
			files = append(files, *v.Path.ToString)

			// If the file was renamed, we'll want to run plan in the directory
			// it was moved from as well.
			if v.SrcPath != nil {
				files = append(files, *v.SrcPath.ToString)
			}
		}
		if *changes.IsLastPage {
			break
		}
		nextPageStart = *changes.NextPageStart
	}

	// Now ensure all files are unique.
	hash := make(map[string]bool)
	var unique []string
	for _, f := range files {
		if !hash[f] {
			unique = append(unique, f)
			hash[f] = true
		}
	}
	return unique, nil
}

func (b *Client) GetProjectKey(repoName string, cloneURL string) (string, error) {
	// Get the project key out of the repo clone URL.
	// Given http://bitbucket.corp:7990/scm/at/atlantis-example.git
	// we want to get 'at'.
	expr := fmt.Sprintf(".*/(.*?)/%s\\.git", repoName)
	capture, err := regexp.Compile(expr)
	if err != nil {
		return "", errors.Wrapf(err, "constructing regex from %q", expr)
	}
	matches := capture.FindStringSubmatch(cloneURL)
	if len(matches) != 2 {
		return "", fmt.Errorf("could not extract project key from %q, regex returned %q", cloneURL, strings.Join(matches, ","))
	}
	return matches[1], nil
}

// CreateComment creates a comment on the merge request. It will write multiple
// comments if a single comment is too long.
func (b *Client) CreateComment(logger logging.SimpleLogging, repo models.Repo, pullNum int, comment string, _ string) error {
	sepEnd := "\n```\n**Warning**: Output length greater than max comment size. Continued in next comment."
	sepStart := "Continued from previous comment.\n```diff\n"
	comments := common.SplitComment(comment, maxCommentLength, sepEnd, sepStart, 0, "")
	for _, c := range comments {
		if err := b.postComment(repo, pullNum, c); err != nil {
			return err
		}
	}
	return nil
}

func (b *Client) ReactToComment(_ logging.SimpleLogging, _ models.Repo, _ int, _ int64, _ string) error {
	return nil
}

func (b *Client) HidePrevCommandComments(_ logging.SimpleLogging, _ models.Repo, _ int, _ string, _ string) error {
	return nil
}

// postComment actually posts the comment. It's a helper for CreateComment().
func (b *Client) postComment(repo models.Repo, pullNum int, comment string) error {
	bodyBytes, err := json.Marshal(map[string]string{"text": comment})
	if err != nil {
		return errors.Wrap(err, "json encoding")
	}
	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/comments", b.BaseURL, projectKey, repo.Name, pullNum)
	_, err = b.makeRequest("POST", path, bytes.NewBuffer(bodyBytes))
	return err
}

// PullIsApproved returns true if the merge request was approved.
func (b *Client) PullIsApproved(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) (approvalStatus models.ApprovalStatus, err error) {
	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return approvalStatus, err
	}
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d", b.BaseURL, projectKey, repo.Name, pull.Num)
	resp, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return approvalStatus, err
	}
	var pullResp PullRequest
	if err := json.Unmarshal(resp, &pullResp); err != nil {
		return approvalStatus, errors.Wrapf(err, "Could not parse response %q", string(resp))
	}
	if err := validator.New().Struct(pullResp); err != nil {
		return approvalStatus, errors.Wrapf(err, "API response %q was missing fields", string(resp))
	}
	for _, reviewer := range pullResp.Reviewers {
		if *reviewer.Approved {
			return models.ApprovalStatus{
				IsApproved: true,
			}, nil
		}
	}
	return approvalStatus, nil
}

func (b *Client) DiscardReviews(_ logging.SimpleLogging, _ models.Repo, _ models.PullRequest) error {
	// TODO implement
	return nil
}

// PullIsMergeable returns true if the merge request has no conflicts and can be merged.
func (b *Client) PullIsMergeable(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest, _ string, _ []string) (bool, error) {
	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/merge", b.BaseURL, projectKey, repo.Name, pull.Num)
	resp, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return false, err
	}
	var mergeStatus MergeStatus
	if err := json.Unmarshal(resp, &mergeStatus); err != nil {
		return false, errors.Wrapf(err, "Could not parse response %q", string(resp))
	}
	if err := validator.New().Struct(mergeStatus); err != nil {
		return false, errors.Wrapf(err, "API response %q was missing fields", string(resp))
	}
	if *mergeStatus.CanMerge && !*mergeStatus.Conflicted {
		return true, nil
	}
	return false, nil
}

// UpdateStatus updates the status of a commit.
func (b *Client) UpdateStatus(logger logging.SimpleLogging, _ models.Repo, pull models.PullRequest, status models.CommitStatus, src string, description string, url string) error {
	bbState := "FAILED"
	switch status {
	case models.PendingCommitStatus:
		bbState = "INPROGRESS"
	case models.SuccessCommitStatus:
		bbState = "SUCCESSFUL"
	case models.FailedCommitStatus:
		bbState = "FAILED"
	}

	logger.Info("Updating BitBucket commit status for '%s' to '%s'", src, bbState)

	// URL is a required field for bitbucket statuses. We default to the
	// Atlantis server's URL.
	if url == "" {
		url = b.AtlantisURL
	}

	bodyBytes, err := json.Marshal(map[string]string{
		"key":         src,
		"url":         url,
		"state":       bbState,
		"description": description,
	})

	path := fmt.Sprintf("%s/rest/build-status/1.0/commits/%s", b.BaseURL, pull.HeadCommit)
	if err != nil {
		return errors.Wrap(err, "json encoding")
	}
	_, err = b.makeRequest("POST", path, bytes.NewBuffer(bodyBytes))
	return err
}

// MergePull merges the pull request.
func (b *Client) MergePull(logger logging.SimpleLogging, pull models.PullRequest, pullOptions models.PullRequestOptions) error {
	projectKey, err := b.GetProjectKey(pull.BaseRepo.Name, pull.BaseRepo.SanitizedCloneURL)
	if err != nil {
		return err
	}

	// We need to make a get pull request API call to get the correct "version".
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d", b.BaseURL, projectKey, pull.BaseRepo.Name, pull.Num)
	resp, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return err
	}
	var pullResp PullRequest
	if err := json.Unmarshal(resp, &pullResp); err != nil {
		return errors.Wrapf(err, "Could not parse response %q", string(resp))
	}
	if err := validator.New().Struct(pullResp); err != nil {
		return errors.Wrapf(err, "API response %q was missing fields", string(resp))
	}
	path = fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/merge?version=%d", b.BaseURL, projectKey, pull.BaseRepo.Name, pull.Num, *pullResp.Version)
	_, err = b.makeRequest("POST", path, nil)
	if err != nil {
		return err
	}
	if pullOptions.DeleteSourceBranchOnMerge {
		bodyBytes, err := json.Marshal(DeleteSourceBranch{Name: "refs/heads/" + pull.HeadBranch, DryRun: false})
		if err != nil {
			return errors.Wrap(err, "json encoding")
		}

		path = fmt.Sprintf("%s/rest/branch-utils/1.0/projects/%s/repos/%s/branches", b.BaseURL, projectKey, pull.BaseRepo.Name)
		_, err = b.makeRequest("DELETE", path, bytes.NewBuffer(bodyBytes))
		if err != nil {
			return err
		}
	}
	return err
}

// MarkdownPullLink specifies the character used in a pull request comment.
func (b *Client) MarkdownPullLink(pull models.PullRequest) (string, error) {
	return fmt.Sprintf("#%d", pull.Num), nil
}

// prepRequest adds auth and necessary headers.
func (b *Client) prepRequest(method string, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}

	// Personal access tokens can be sent as basic auth or bearer
	bearer := "Bearer " + b.Password
	req.Header.Add("Authorization", bearer)

	if body != nil {
		req.Header.Add("Content-Type", "application/json")
	}
	// Add this header to disable CSRF checks.
	// See https://confluence.atlassian.com/cloudkb/xsrf-check-failed-when-calling-cloud-apis-826874382.html
	req.Header.Add("X-Atlassian-Token", "no-check")
	return req, nil
}

func (b *Client) makeRequest(method string, path string, reqBody io.Reader) ([]byte, error) {
	req, err := b.prepRequest(method, path, reqBody)
	if err != nil {
		return nil, errors.Wrap(err, "constructing request")
	}
	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() // nolint: errcheck
	requestStr := fmt.Sprintf("%s %s", method, path)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != 204 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("making request %q unexpected status code: %d, body: %s", requestStr, resp.StatusCode, string(respBody))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "reading response from request %q", requestStr)
	}
	return respBody, nil
}

// GetTeamNamesForUser returns the names of the teams or groups that the user belongs to (in the organization the repository belongs to).
func (b *Client) GetTeamNamesForUser(_ logging.SimpleLogging, _ models.Repo, _ models.User) ([]string, error) {
	return nil, nil
}

func (b *Client) SupportsSingleFileDownload(_ models.Repo) bool {
	return false
}

// GetFileContent a repository file content from VCS (which support fetch a single file from repository)
// The first return value indicates whether the repo contains a file or not
// if BaseRepo had a file, its content will placed on the second return value
func (b *Client) GetFileContent(_ logging.SimpleLogging, _ models.PullRequest, _ string) (bool, []byte, error) {
	return false, []byte{}, fmt.Errorf("not implemented")
}

func (b *Client) GetCloneURL(_ logging.SimpleLogging, _ models.VCSHostType, _ string) (string, error) {
	return "", fmt.Errorf("not yet implemented")
}

func (b *Client) GetPullLabels(_ logging.SimpleLogging, _ models.Repo, _ models.PullRequest) ([]string, error) {
	return nil, fmt.Errorf("not yet implemented")
}
