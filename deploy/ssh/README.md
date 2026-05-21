# Deploy SSH credentials

This directory holds a **dedicated** SSH keypair the worker container uses to
push and clone from Gitea / GitHub. It is mounted into the worker by
`deploy/docker-compose.yml`. The operator's broader `~/.ssh` is no longer
exposed (see [#221] and `docs/security-posture.md`).

[#221]: https://github.com/xrf9268-hue/aiops-platform/issues/221

## One-time setup

```bash
cd deploy
ssh-keygen -t ed25519 -f ssh/id_ed25519 -C aiops-worker-deploy-key -N ''
ssh-keyscan -H <your-gitea-host> >> ssh/known_hosts
# Then add ssh/id_ed25519.pub as a deploy key in the target Gitea/GitHub repo.
```

## Overrides

Set `AIOPS_SSH_KEY_PATH` and/or `AIOPS_SSH_KNOWN_HOSTS_PATH` in your `.env`
(or shell) to point at a different keypair location. Paths are resolved
relative to `deploy/docker-compose.yml`.

```dotenv
# Example: keep the deploy key under XDG state instead of inside the repo.
AIOPS_SSH_KEY_PATH=${HOME}/.local/state/aiops/id_ed25519
AIOPS_SSH_KNOWN_HOSTS_PATH=${HOME}/.local/state/aiops/known_hosts
```

## Safety

- The repository's root `.gitignore` ignores everything in this directory
  except this README and `.gitkeep`. **Never** commit a private key.
- Use a **dedicated** keypair scoped to the workflow's repo set. Do not
  reuse your personal `~/.ssh/id_*` here — that defeats the purpose of #221.
- Rotate the keypair periodically; on rotation, regenerate `known_hosts` if
  the destination's host key changed.
