// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v29/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

type Environment interface {
	BenchFunc() string
	CompareTarget() string

	PostErr(err string) error
	PostResults(cmps []BenchCmp) error

	Repo() *git.Repository
}

type environment struct {
	logger Logger

	benchFunc     string
	compareTarget string
}

func (e environment) BenchFunc() string     { return e.benchFunc }
func (e environment) CompareTarget() string { return e.compareTarget }

type Local struct {
	environment

	repo *git.Repository
}

func newLocalEnv(e environment) (Environment, error) {
	r, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, err
	}

	return &Local{environment: e, repo: r}, nil
}

func (l *Local) PostErr(string) error { return nil } // Noop. We will see error anyway.

func (l *Local) PostResults(cmps []BenchCmp) error {
	fmt.Println("Results:")
	Render(os.Stdout, cmps, false, false, l.compareTarget)
	return nil
}

func (l *Local) Repo() *git.Repository { return l.repo }

// TODO: Add unit test(!).
type GitHubActions struct {
	environment

	repo   *git.Repository
	client *gitHubClient
}

func newGitHubActionsEnv(ctx context.Context, e environment, owner, repo string, prNumber int) (Environment, error) {
	workspace, ok := os.LookupEnv("GITHUB_WORKSPACE")
	if !ok {
		return nil, errors.New("funcbench is not running inside GitHub Actions")
	}

	r, err := git.PlainCloneContext(ctx, fmt.Sprintf("%s/%s", workspace, repo), false, &git.CloneOptions{
		URL:      fmt.Sprintf("https://github.com/%s/%s.git", owner, repo),
		Progress: os.Stdout,
		Depth:    1,
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not clone repository")
	}

	if err := os.Chdir(filepath.Join(workspace, repo)); err != nil {
		return nil, errors.Wrapf(err, "changing to %s/%s dir", workspace, repo)
	}

	ghClient, err := newGitHubClient(owner, repo, prNumber)
	if err != nil {
		return nil, errors.Wrapf(err, "couldn't create github client")
	}
	g := &GitHubActions{
		environment: e,
		repo:        r,
		client:      ghClient,
	}

	wt, err := g.repo.Worktree()
	if err != nil {
		return nil, err
	}

	if err := r.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+refs/pull/%d/head:refs/heads/pullrequest", prNumber)),
		},
		Progress: os.Stdout,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		if pErr := g.PostErr("Switch (fetch) to a pull request branch failed"); pErr != nil {
			return nil, errors.Wrapf(err, "posting a comment for `checkout` command execution error; postComment err:%v", pErr)
		}
		return nil, err
	}

	if err = wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("pullrequest"),
	}); err != nil {
		if pErr := g.PostErr("Switch to a pull request branch failed"); pErr != nil {
			return nil, errors.Wrapf(err, "posting a comment for `checkout` command execution error; postComment err:%v", pErr)
		}
		return nil, err
	}
	return g, nil
}

func (g *GitHubActions) PostErr(err string) error {
	if err := g.client.postComment(fmt.Sprintf("%v. Benchmark did not complete, please check action logs.", err)); err != nil {
		return errors.Wrap(err, "posting err")
	}
	return nil
}

func (g *GitHubActions) PostResults(cmps []BenchCmp) error {
	b := bytes.Buffer{}
	Render(&b, cmps, false, false, g.compareTarget)
	return g.client.postComment(formatCommentToMD(b.String()))
}

func (g *GitHubActions) Repo() *git.Repository { return g.repo }

func formatCommentToMD(rawTable string) string {
	tableContent := strings.Split(rawTable, "\n")
	for i := 0; i <= len(tableContent)-1; i++ {
		e := tableContent[i]
		switch {
		case e == "":

		case strings.Contains(e, "old ns/op"):
			e = "| Benchmark | Old ns/op | New ns/op | Delta |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		case strings.Contains(e, "old MB/s"):
			e = "| Benchmark | Old MB/s | New MB/s | Speedup |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		case strings.Contains(e, "old allocs"):
			e = "| Benchmark | Old allocs | New allocs | Delta |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		case strings.Contains(e, "old bytes"):
			e = "| Benchmark | Old bytes | New bytes | Delta |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		default:
			// Replace spaces with "|".
			e = strings.Join(strings.Fields(e), "|")
		}
		tableContent[i] = e
	}
	return strings.Join(tableContent, "\n")

}

type gitHubClient struct {
	owner    string
	repo     string
	prNumber int
	client   *github.Client
}

func newGitHubClient(owner, repo string, prNumber int) (*gitHubClient, error) {
	// TODO: add context
	ghToken, ok := os.LookupEnv("GITHUB_TOKEN")
	if !ok {
		return nil, fmt.Errorf("GITHUB_TOKEN missing")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: ghToken})
	tc := oauth2.NewClient(context.Background(), ts)
	c := gitHubClient{
		client:   github.NewClient(tc),
		owner:    owner,
		repo:     repo,
		prNumber: prNumber,
	}
	return &c, nil
}

func (c *gitHubClient) postComment(comment string) error {
	issueComment := &github.IssueComment{Body: github.String(comment)}
	_, _, err := c.client.Issues.CreateComment(context.Background(), c.owner, c.repo, c.prNumber, issueComment)
	return err
}
