# release-bot — automation with a real token the workload never sees

**The problem.** Release automation needs a GitHub token to open the release
PR, comment on issues, and fetch a changelog. But the code doing the work, such
as changelog generation, version bumping, and scripts from the repo, is exactly
the kind of code you should not hand a credential to. Tokens in the environment
end up in `/proc`, in crash dumps, in child processes, in `curl` argv.

**The move.** Store the token in Vora's broker (`vora secrets set`), run the
automation in a sandbox with `--broker github`, and have the workload call
`$VORA_BROKER_URL/creds/github/<path>`. The gateway attaches the `Authorization`
header upstream over TLS. The value never enters the sandbox: not the
environment, not the filesystem, not the persisted record, not the audit trail
(which records only the *alias*). A workload that misbehaves can talk only to
`api.github.com`. The broker is bound to that one host and never follows
redirects ([ADR 013](../../docs/adr/013-credential-brokering.md)).

## Run it

This recipe needs the control plane with the secret store configured
(`VORA_MASTER_KEY` on `vora-api`):

```bash
export VORA_MASTER_KEY="$(openssl rand -base64 48)"   # on vora-api
vora-api &

# Operator: store the token once, bound to api.github.com.
printf '%s' "$GITHUB_TOKEN" | vora --api "$VORA_API" --api-key "$VORA_ADMIN_KEY" \
  secrets set --tenant t1 --alias github --host api.github.com --value-stdin

./run.sh
```

`run.sh` starts the automation with `--broker github`, has it fetch the
authenticated `/user` endpoint *through the broker*, then proves the boundary:
the same request without the broker fails, the sandbox holds no token, and the
audit trail names the alias.

## What to notice

- The workload uses plain `curl "$VORA_BROKER_URL/creds/github/user"`, with no
  SDK and no token handling. Change of plans? Revoke the alias; the workload keeps
  working with whatever the broker allows.
- Try `curl https://api.github.com/user` directly from the sandbox: no
  credentials arrive (and egress is denied outside the allowlist anyway).
- `vora logs "$sbx" --egress` shows `allowed alias=github api.github.com`, which
  records who used the credential and when, with zero secret material. That record
  makes brokered automation auditable.
