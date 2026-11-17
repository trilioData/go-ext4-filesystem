# go-ext4-filesystem (Trilio fork)

Read-only ext4 reader used by the k8s-triliovault filesystem restore. It reads
a backed-up ext4 image over NBD and the restore writes the tree back to a PV.

## Branches

- `master` is the Trilio branch. Work and pull requests target `master`.
- `main` mirrors the upstream fork. Do not merge `main` into `master`:
  cherry-pick the upstream commits that are needed (`git cherry-pick -x`), and
  check first whether `master` already covers the fix in its own way.

## CHANGES-trilio.md

`CHANGES-trilio.md` records everything this fork changes from upstream. Read
the relevant section before changing code it describes.

Every change to the library goes into it in the same pull request:

- a row in the inventory table (§0), and
- a numbered section with an "In plain terms" part listing the issues and the
  changes. Extend an existing section when the change belongs to it.

Also record upstream commits that were looked at and deliberately not taken,
with the reason.

## Tests come first

When changing behaviour or fixing a bug, the first step is the test, before
the code change:

1. Check whether a test already covers the behaviour being changed.
2. If not, add one first. For a bug fix it must fail on the current code and
   pass after the fix.
3. Then make the change.

Where tests go:

- `ext4/*_test.go`: unit tests on hand-built images and inodes.
- `ext4/image_test.go`, `ext4/api_test.go`: tests against real images built
  with `mke2fs -d` and `debugfs` (no root needed). Use these when the
  behaviour depends on what real ext4 tools write.
- `e2e/`, `scripts/e2e-clone.sh`: end-to-end clone against a kernel mount.
  Needs root.

## Before finishing

```sh
gofmt -l ext4 e2e                      # must print nothing
go vet ./... && go vet -tags e2e ./e2e/
go test -race -count=1 ./...
sudo scripts/e2e-clone.sh              # root only; CI runs it otherwise
```

The image tests need e2fsprogs and skip without it. Say so when they were
skipped, and when the e2e clone was not run.

## Constraints

- `go.mod` declares Go 1.18: no `min`, `max` or `clear` builtins.
- The library is read-only and must never serve bytes from outside the
  filesystem. Damaged input returns an error, never a panic.
- Results that a restore recreates are returned verbatim (symlink targets,
  xattr values). Do not clean or normalise them.
