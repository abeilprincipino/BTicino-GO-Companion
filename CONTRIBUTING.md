# Contributing

## Branches

| Branch | Purpose |
| --- | --- |
| `develop` | Default branch. Every pull request targets it. Beta releases are cut from it. |
| `main` | What official releases are built from. The installer and uninstaller are downloaded from it, so it only receives changes that have already shipped in a beta. |

Feature branches are created from `develop`, for example `feat/auxiliary-unlock` or
`fix/ring-video-timing`.

## Pull requests

1. Branch from `develop` and open the pull request against `develop`.
2. The **CI** workflow runs `go vet`, `go test ./...` and an ARMv7 build on Linux. It must pass
   before the pull request can be merged. Parts of `internal/media` only build and test on Linux,
   so a green run on Windows or macOS is not enough.
3. Say in the description whether the change was tried on a real intercom, and on which model
   (Classe 300X or 100X). Tests cannot confirm that a door opens or that video arrives.
4. Configuration changes must keep old `config.yaml` files loading with unchanged behaviour. Say
   whether a build older than yours can still read a configuration your change has saved.

## Releases

Releases are published by the **Release** workflow. It runs the tests, builds the ARMv7 bundle and
publishes `companion.tar.gz` on GitHub Releases. There are two ways to start it:

- **From GitHub:** open **Actions > Release > Run workflow**, pick the branch in **Use workflow from**
  and type the new tag. The workflow creates the tag on the head of that branch, but only after the
  tests and the build pass, so a failed run leaves no tag behind. A tag that already exists is
  refused.
- **From a clone:** push the tag yourself, as shown below.

### Beta

A beta is a tag with a pre-release suffix on a commit of `develop`. From GitHub, run the workflow
from `develop` with a tag such as `v0.12.0-beta.1`. From a clone:

```sh
git checkout develop && git pull
git tag -a v0.12.0-beta.1 -m "v0.12.0-beta.1"
git push origin v0.12.0-beta.1
```

It is published as a GitHub pre-release, which companions on the **Beta** update channel install.
Betas are cut by hand when `develop` is worth testing, not on every merge. A tag that fails the
workflow publishes nothing; cut the next `-beta.N` instead of moving a pushed tag.

### Official release

Once a beta has been validated on real hardware:

1. Open a pull request from `develop` to `main` and merge it with **Create a merge commit**, so the
   two branches keep a shared history.
2. Tag the merge commit on `main` without a suffix: run the workflow from `main` with a tag such
   as `v0.12.0`, or from a clone:

   ```sh
   git checkout main && git pull
   git tag -a v0.12.0 -m "v0.12.0"
   git push origin v0.12.0
   ```

The Release workflow refuses a tag without a suffix that does not point to a commit on `main`, and
a beta tag that points to neither `develop` nor `main`.

### Hotfix

A fix that cannot wait for `develop` goes to `main` through a pull request from a branch created
from `main`. Tag it on `main` (optionally as a beta first), then merge `main` back into `develop`.
