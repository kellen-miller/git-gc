# git-gc

## Description

This is a simple script to run `git gc` on all git repositories in a directory.

## Installation

`go install github.com/kellen-miller/git-gc/cmd/git-gc@latest`

## Flags

- `--root` - The directory to search for git repositories in. Defaults to the users home directory.
- `--parallel` - The number of repositories to run `git gc` on in parallel. Defaults to number of CPUs.