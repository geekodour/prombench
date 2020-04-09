# funcbench

Benchmark and compare your Go code between commits or sub benchmarks. It uses `go test -bench` to run the benchmarks and uses [benchcmp](https://godoc.org/golang.org/x/tools/cmd/benchcmp) to compare them.

funcbench can be run locally in the command line aswell as a Github Action. Running it in the Github Action environment also allows it to accept *a pull request number* and *a branch/commit* to compare against, which makes it suitable for automated tests.

### Environment variables
> Any variable starting with `GITHUB_` is not required when running locally.
- `GITHUB_WORKSPACE`: This already is set when running in GitHub Actions, we can set this to a desired directory if we're trying to emulate the Github Actions environment, eg. when running in GKE.
- `GITHUB_TOKEN`: Access token to post benchmarks results to respective PR.

## Usage Examples
> Clean git state is required.

|usage|command|
|--|--|
|Execute benchmark named `BenchmarkFuncName` regex, and compare it with `master` branch. | ``` ./funcbench -v master BenchmarkFuncName ``` |
|Execute all benchmarks matching `BenchmarkFuncName.*` regex, and compare it with `master` branch.|```./funcbench -v master BenchmarkFuncName.*```|

* Execute all benchmarks, and compare the results with `devel` branch.

```
./funcbench -v devel .
```
```
./funcbench -v devel
```

* Execute all benchmarks matching `BenchmarkFuncName.*` regex, and compare it with `6d280faa16bfca1f26fa426d863afbb564c063d1` commit.

```
./funcbench -v 6d280faa16bfca1f26fa426d863afbb564c063d1 BenchmarkFuncName.*
```

* Execute all benchmarks matching `BenchmarkFuncName.*` regex on current code. Compare it between sub-benchmarks (`b.Run`) of same benchmark for current commit. Errors out if there are no sub-benchmarks.

```
./funcbench -v . FuncName.*
```

## Triggering with GitHub comments
The benchmark can be triggered by creating a comment which specifies a branch to compare. The results are then posted back as a PR comment.

Tests are triggered by posting a comment in a PR with the following format:

`/funcbench <branch/commit> <Go test regex>`

Specifying which tests to run are filtered by using the standard [Go regex RE2 language](https://github.com/google/re2/wiki/Syntax).

* To test it locally, set `-w` flag or `WORKSPACE` environment variable to an empty directory where the source will be cloned.

#### Example Github actions workflow file

```
on: issue_comment // Workflow is executed when a pull request comment is created.
name: Benchmark
jobs:
  commentMonitor:
    runs-on: ubuntu-latest
    steps:
    - name: commentMonitor
      uses: docker://prominfra/comment-monitor:latest
      env:
        COMMENT_TEMPLATE: 'The benchmark has started.' // Body of a comment that is created to announce start of a benchmark.
        GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }} // Github secret token/
      with:
        args: '"^/funcbench ?(?P<BRANCH>[^ B\.]+)? ?(?P<REGEX>\.|Bench.*|[^ ]+)?'
    - name: benchmark
      uses: docker://prominfra/funcbench:latest
      env:
        GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }} // Github secret token/
```

## Building Docker container.

From the repository root:

`docker build -t <tag of your choice> -f funcbench/Dockerfile .`
