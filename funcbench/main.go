// Copyright 2019 The Prometheus Authors
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
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/run"
	"github.com/pkg/errors"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

type Logger interface {
	Println(v ...interface{})
}

type logger struct {
	*log.Logger

	verbose bool
}

func (l *logger) FatalError(err error) {
	if l.verbose {
		l.Fatalf("%+v", err)
	}
	l.Fatalf("%v", err)
}

func main() {
	cfg := struct {
		verbose        bool
		owner          string
		repo           string
		resultsDir     string
		ghPr           int
		benchTime      time.Duration
		benchTimeout   time.Duration
		compareTarget  string
		benchFuncRegex string
	}{}

	app := kingpin.New(
		filepath.Base(os.Args[0]),
		"Benchmark and compare your Go code between sub benchmarks or commits.",
	)
	app.HelpFlag.Short('h')
	app.Flag("verbose", "Verbose mode. Errors includes trace and commands output are logged.").
		Short('v').BoolVar(&cfg.verbose)

	app.Flag("owner", "A Github owner or organisation name.").
		Default("prometheus").StringVar(&cfg.owner)
	app.Flag("repo", "This is the repository name.").
		Default("prometheus").StringVar(&cfg.repo)
	app.Flag("github-pr", "GitHub PR number to pull changes from and to post benchmark results.").
		IntVar(&cfg.ghPr) // FIXME: will this create a branch with the PR name??
		// FIXME: worktree??
	app.Flag("result-cache", "Directory to store benchmark results. Useful for local runs. ??? FIXME ").
		Default("_dev/funcbench").
		StringVar(&cfg.resultsDir) // TODO: probably should have a default.

	app.Flag("bench-time", " FIXME ").
		Short('t').Default("1s").DurationVar(&cfg.benchTime)
	app.Flag("timeout", "Benchmark timeout specified in time.Duration format, "+
		"disabled if set to 0. If test binary runs longer than timeout duration, raise panic.").
		Default("2h").DurationVar(&cfg.benchTimeout) //FIXME probably should error out and not panic

	app.Arg("target", "Can be one of '.', branch name or commit SHA of the branch "+
		"to compare against. If set to '.', branch/commit is the same as the current one; "+
		"funcbench will run once and try to compare between 2 sub-benchmarks. "+
		"Errors out if there are no sub-benchmarks.").
		Required().StringVar(&cfg.compareTarget) // FIXME: can this be the commit of a branch that's not checked out
	app.Arg("function-regex", "Function to use for benchmark. Supports RE2 regexp or `.` "+
		"to run all benchmarks.").
		Required().
		StringVar(&cfg.benchFuncRegex) // FIXME: can we use Default("") instead of having to make this Required.

	kingpin.MustParse(app.Parse(os.Args[1:]))

	logger := &logger{
		// Show file line with each log.
		Logger:  log.New(os.Stdout, "funcbech", log.Ltime|log.Lshortfile),
		verbose: cfg.verbose,
	}

	var g run.Group
	// Main routine.
	{
		ctx, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			var (
				env Environment
				err error
			)

			e := environment{
				logger:        logger,
				benchFunc:     cfg.benchFuncRegex,
				compareTarget: cfg.compareTarget,
			}

			if cfg.ghPr == 0 {
				// Local Environment
				// TODO (geekodour): GKE funcbench should be running on this environment.
				env, err = newLocalEnv(e)
				if err != nil {
					return errors.Wrap(err, "new env") // FIXME: improve error message.
				}
				// TODO (geekodour): do better logging for when all benchmarks are running.
				logger.Printf("funcbench start [Local Mode]: Benchmarking current version versus %q for benchmark funcs: %q\n", cfg.compareTarget, cfg.benchFuncRegex)
			} else {
				// Github Actions Environment
				env, err = newGitHubActionsEnv(ctx, e, cfg.owner, cfg.repo, cfg.ghPr)
				if err != nil {
					return errors.Wrap(err, "new env")
				}
				logger.Printf("funcbench start [GitHub Mode]: Benchmarking %q (PR-%d) versus %q for benchmark funcs: %q\n", fmt.Sprintf("%s/%s", cfg.owner, cfg.repo), cfg.ghPr, cfg.compareTarget, cfg.benchFuncRegex)
			}

			// ( ◔_◔)ﾉ Start benchmarking!
			return startBenchmark(ctx, env, newBenchmarker(logger, env, &commander{verbose: cfg.verbose}, cfg.benchTime, cfg.benchTimeout, cfg.resultsDir))
			// TODO (geekodour): post a funchbench summary, how long it took etc both in the comment and local setting.

		}, func(err error) {
			cancel()
		})
	}
	// Listen for termination signals.
	{
		cancel := make(chan struct{})
		g.Add(func() error {
			return interrupt(logger, cancel)
		}, func(error) {
			close(cancel)
		})
	}

	if err := g.Run(); err != nil {
		logger.FatalError(errors.Wrap(err, "running command failed"))
	}
	logger.Println("exiting")
}

// startBenchmark  TODO
func startBenchmark(
	ctx context.Context,
	env Environment,
	bench *Benchmarker,
) error {
	var oldResult, newResult string

	wt, _ := env.Repo().Worktree()
	ref, err := env.Repo().Head()
	if err != nil {
		return errors.Wrap(err, "get head")
	}

	if _, err := bench.c.exec("bash", "-c", "git update-index -q --ignore-submodules --refresh && git diff-files --quiet --ignore-submodules --"); err != nil {
		return errors.Wrap(err, "not clean worktree")
	}

	// 1. Execute benchmark against packages in the current directory.
	newResult, err = bench.execBenchmark(wt.Filesystem.Root(), ref.Hash())
	if err != nil {
		// TODO(bwplotka): Just defer posting all errors?
		if pErr := env.PostErr("Go bench test for this pull request failed"); pErr != nil {
			return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
		}
		return errors.Wrap(err, "exec benchmark A")
	}

	targetCommit, compareWithItself, err := compareTargetRef(ctx, env.Repo(), env.CompareTarget())
	if err != nil {
		return errors.Wrap(err, "compareTargetRef")
	}

	if compareWithItself {
		bench.logger.Println("Target:", env.CompareTarget(), "is `.`; Assuming sub-benchmarks comparison.")

		// 2a. Compare sub benchmarks. TODO.
		cmps, err := bench.compareSubBenchmarks(newResult)
		if err != nil {
			if pErr := env.PostErr("`benchcmp` failed."); pErr != nil {
				return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
			}
			return errors.Wrap(err, "compare sub benchmarks")
		}
		return errors.Wrap(env.PostResults(cmps), "post results")
	}

	bench.logger.Println("Target:", env.CompareTarget(), "is evaluated to be ", targetCommit.String(), ". Assuming comparing with this one (clean workdir will be checked.)")
	// 2b. Compare with target commit/branch.

	// 3. Best effort cleanup of worktree.
	cmpWorkTreeDir := filepath.Join(wt.Filesystem.Root(), "_funcbench-cmp")

	_, _ = bench.c.exec("git", "worktree", "remove", cmpWorkTreeDir)
	bench.logger.Println("Checking out (in new workdir):", cmpWorkTreeDir, "commmit", ref.String())
	if _, err := bench.c.exec("git", "worktree", "add", "-f", cmpWorkTreeDir, ref.Hash().String()); err != nil {
		return errors.Wrapf(err, "failed to checkout %s in worktree %s", ref.String(), cmpWorkTreeDir)
	}

	// 4. Benchmark in new worktree.
	oldResult, err = bench.execBenchmark(cmpWorkTreeDir, targetCommit)
	if err != nil {
		if pErr := env.PostErr(fmt.Sprintf("Go bench test for target %s failed", env.CompareTarget())); pErr != nil {
			return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
		}
		return errors.Wrap(err, "exec bench B")
	}

	// 5. Compare old vs new.
	cmps, err := bench.compareBenchmarks(oldResult, newResult)
	if err != nil {
		if pErr := env.PostErr("`benchcmp` failed."); pErr != nil {
			return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
		}
		return errors.Wrap(err, "compare benchmarks")
	}
	return errors.Wrap(env.PostResults(cmps), "post results")
}

func interrupt(logger Logger, cancel <-chan struct{}) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-c:
		logger.Println("caught signal", s, "Exiting.")
		return nil
	case <-cancel:
		return errors.New("canceled")
	}
}

func compareTargetRef(ctx context.Context, repo *git.Repository, target string) (ref plumbing.Hash, compareWithItself bool, _ error) {
	if target == "." {
		return plumbing.Hash{}, true, nil
	}
	currRef, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash, false, err
	}

	if target == strings.TrimPrefix(currRef.Name().String(), "refs/heads/") || target == currRef.Hash().String() {
		return currRef.Hash(), true, errors.Errorf("target: %s is the same as current ref %s (or is on the same commit); No changes would be expected; Aborting", target, currRef.String())
	}

	targetRef, err := repo.Reference(plumbing.NewBranchReferenceName(target), false)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}

	return targetRef.Hash(), false, nil
}

type commander struct {
	verbose bool
}

func (c *commander) exec(command ...string) (string, error) {
	// TODO(bwplotka): Use context to kill command on interrupt.
	cmd := exec.Command(command[0], command[1:]...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b

	if c.verbose {
		// All to stdout.
		cmd.Stdout = io.MultiWriter(cmd.Stdout, os.Stdout)
		cmd.Stderr = io.MultiWriter(cmd.Stdout, os.Stdout)
	}
	if err := cmd.Run(); err != nil {
		out := b.String()
		if c.verbose {
			out = ""
		}
		return "", errors.Errorf("error: %v; Command out: %s", err, out)
	}

	return b.String(), nil
}
