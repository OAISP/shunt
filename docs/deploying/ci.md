---
title: Deploying from CI
parent: Deploying
nav_order: 4
---

# Deploying from CI

A CI job is another machine running `shunt up`, so nothing here is required.
Four settings make it work well, and the first matters more than the rest put
together.

## Give the build a cache

A CI runner starts with an empty builder. Without a cache, every layer after the
base image is rebuilt, gets a new digest and is shipped again in full — and
since the digest changed, every pipeline also looks like a change and replaces
the containers. The deduplication shunt exists for never happens.

`SHUNT_BUILD_CACHE` fixes that:

| Value | Cache |
|:--|:--|
| `gha` | GitHub Actions' cache service. |
| a directory | A directory your CI keeps between jobs. Each build writes a fresh cache beside the old one and swaps it in, so it does not grow. |

Measured on a 17.8 MB image with one `RUN` layer, using a new builder for every
job, as a CI runner does:

| | On the wire | Deployed |
|:--|--:|:--|
| No cache, no source change | 14.4 MB | yes, needlessly |
| No cache, one-line change | 14.4 MB | yes |
| Cache, no source change | 0 | no — "nothing to do" |
| Cache, one-line change | 4.2 KB | yes |

The cache is shared by every target of a manifest, since they build the same
images.

## Run one deploy per target at a time

shunt already serialises deploys on the server and refuses one whose plan went
stale while it waited. Let the CI system queue them too, so that never has to
happen:

- **GitHub Actions:** `concurrency: { group: deploy-production, cancel-in-progress: false }`.
  A newer run waiting in the group replaces an older one that has not started.
- **GitLab CI:** `resource_group: production`, with the group's process mode set
  to `newest_first` so a backlog collapses to the latest commit.

Cancelling a job is safe. Before the release reaches the server nothing has
changed; after, the server finishes the deploy and records it. See
[If the connection drops]({% link deploying/index.md %}#if-the-connection-drops).

## Give it ssh, and pin the host key

shunt runs the system `ssh` non-interactively, so the job needs a key that
works without a prompt and the server's key in `known_hosts`. Store the host key
as a CI variable rather than running `ssh-keyscan` in the job, which would trust
whatever answers:

```sh
ssh-keyscan -H vps.example.com    # once, from a machine you trust
```

Use a key for CI only, and remember what it can do: ssh access to a user in the
`docker` group is root on that server. Gate production behind an approval —
GitHub environments, GitLab protected environments.

## Pass secrets through the environment

The `env` provider reads named variables, which is how CI systems hand secrets
to a job:

```toml
[secrets]
provider = "env"
keys     = ["DATABASE_URL", "STRIPE_KEY"]
```

## GitHub Actions

```yaml
name: deploy
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    concurrency:
      group: deploy-production
      cancel-in-progress: false
    steps:
      - uses: actions/checkout@v4

      - name: install shunt
        run: |
          curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh \
            | SHUNT_VERSION=v0.5.1 PREFIX="$HOME/.local/bin" sh
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"

      # The runner's default builder cannot export an OCI layout.
      - uses: docker/setup-buildx-action@v3
      # Gives buildx the cache service's credentials, for SHUNT_BUILD_CACHE=gha.
      - uses: crazy-max/ghaction-github-runtime@v3

      - name: ssh
        run: |
          install -m 700 -d ~/.ssh
          printf '%s\n' "${{ secrets.DEPLOY_SSH_KEY }}" > ~/.ssh/id_ed25519
          chmod 600 ~/.ssh/id_ed25519
          printf '%s\n' "${{ vars.DEPLOY_KNOWN_HOSTS }}" >> ~/.ssh/known_hosts

      - run: shunt up --rollback-on-failure
        env:
          SHUNT_BUILD_CACHE: gha
          DATABASE_URL: ${{ secrets.DATABASE_URL }}
          STRIPE_KEY: ${{ secrets.STRIPE_KEY }}
```

The release records the commit, the branch, the user who pushed and a link to
the run, so `shunt status` shows where production came from.

## GitLab CI

```yaml
deploy:
  stage: deploy
  image: docker:27
  services: [docker:27-dind]
  variables:
    DOCKER_TLS_CERTDIR: "/certs"
    SHUNT_BUILD_CACHE: .shunt-cache
  cache:
    key: shunt-build
    paths: [.shunt-cache]
  resource_group: production
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
  before_script:
    - apk add --no-cache rsync openssh-client curl
    - curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | SHUNT_VERSION=v0.5.1 sh
    - docker buildx create --use --driver docker-container
    - install -m 700 -d ~/.ssh
    - printf '%s\n' "$DEPLOY_SSH_KEY" > ~/.ssh/id_ed25519 && chmod 600 ~/.ssh/id_ed25519
    - printf '%s\n' "$DEPLOY_KNOWN_HOSTS" > ~/.ssh/known_hosts
  script:
    - shunt up --rollback-on-failure
```

## Build once, promote

`shunt up` builds on every run, which a cache makes cheap — but staging and
production are still two builds. To deploy the same bytes to both, build a
[bundle]({% link deploying/bundles.md %}) once and apply it to each target. The
build job needs no production credentials.

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      # checkout, install shunt, setup-buildx-action, ghaction-github-runtime, as above
      - run: shunt bundle -o release.shuntpkg
        env: { SHUNT_BUILD_CACHE: gha }
      - uses: actions/upload-artifact@v4
        with: { name: release, path: release.shuntpkg }

  staging:
    needs: build
    runs-on: ubuntu-latest
    environment: staging
    concurrency: { group: deploy-staging, cancel-in-progress: false }
    steps:
      - uses: actions/download-artifact@v4
        with: { name: release }
      # install shunt and ssh as above; applying needs no docker
      - run: shunt apply -t staging release.shuntpkg

  production:
    needs: staging
    runs-on: ubuntu-latest
    environment: production    # required reviewers make this the approval step
    concurrency: { group: deploy-production, cancel-in-progress: false }
    steps:
      - uses: actions/download-artifact@v4
        with: { name: release }
      # install shunt and ssh as above
      - run: shunt apply release.shuntpkg
```

Secrets are resolved by the job that applies the bundle, so give each deploy
job the variables its target's provider reads.

## Deploy an image your pipeline already built

If the pipeline already builds, scans and signs the image, rebuilding it in the
deploy job would ship a different image from the one that was checked. Export
the checked image by digest into an OCI layout and let shunt ship that instead;
see [prebuilt images]({% link manifest/images.md %}#prebuilt-images). The deploy
job then needs no Docker and no build cache, only shunt, ssh, rsync and a
registry client. Start from the [GitLab CI](#gitlab-ci) job above, drop
docker, dind and the build cache, install `regctl`, and replace the script:

```yaml
  script:
    - regctl image copy "$IMAGE@$IMAGE_DIGEST" ocidir://.ci/oci/app:release
    - shunt up --rollback-on-failure
```

with `layout = ".ci/oci/app"` under `[images.app]` in `shunt.toml`. The server
never talks to the registry, so it works for a registry the server cannot reach.

## Checks on pull requests

`shunt validate` needs no ssh and no build, so it is safe on every pull request,
forks included. `shunt plan` shows exactly what merging would deploy, but it
builds and connects to the server, so run it only where the deploy key is
available. It exits 0 either way; `--detailed-exitcode` makes it exit 2 when
there are changes.
