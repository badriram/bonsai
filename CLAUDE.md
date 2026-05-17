# Bonsai — context for AI sessions

## Where the design lives

Architecture, decisions, phase plan, and trade-off history live in the
**Banyan trunk `bnyn-ccb8a755`** (title: *Bonsai*, public).

Public link: https://banyan.vamitra.com/trunk/bnyn-ccb8a755

**Always read it before making non-trivial changes.** If you're an AI agent
with Banyan MCP tools available:

```
banyan_checkin({ agent_id: 'code-builder', role: 'code-builder', trunk_id: 'bnyn-ccb8a755' })
banyan_harvest({ node_id: 'bnyn-ccb8a755', depth: 2 })
```

If you're a human, open the public link.

## Branches on the trunk

- **Vision & Design Principles** (`bnyn-e66d7efa`) — why Bonsai exists,
  Tier 1/2/3 capability model, the name.
- **Architecture** (`bnyn-63e81c78`) — provider interface, secret store,
  language decision (Go + aws-sdk-go-v2 direct), terminology, update model.
- **Implementation Plan** (`bnyn-c2edd5cc`) — phased delivery, repo
  structure, CLI surface split, cost profile.

## Locked-in decisions (do not re-litigate without raising a thread)

| Decision               | Choice                                          |
|------------------------|-------------------------------------------------|
| Language               | Go                                              |
| Provisioning           | aws-sdk-go-v2 direct (no CDK / Pulumi / Terraform) |
| Secret store           | Parameter Store SecureString (NOT Secrets Manager) |
| Networking             | Public subnets only, no NAT Gateway             |
| Worker terminology     | "workers" (not "agents")                        |
| Update mechanism       | AMI rebake + ASG refresh; SUC for k3s; kured    |
| Surface split          | Dev: grow/status/logs · Operator: --advanced    |
| License                | Apache 2.0                                      |

To revisit any of these, open a Banyan thread (`banyan_open_thread`) against
the relevant leaf — don't silently change course in code.

## When you finish a session

Save handoff for the next session:

```
banyan_checkin({
  agent_id: 'code-builder',
  trunk_id: 'bnyn-ccb8a755',
  handoff: '<what shipped, what is in progress, what is blocked, what to do first>'
})
```

## Workflow conventions

- Edit existing files over creating new ones.
- No comments unless the *why* is non-obvious. Identifier names carry the *what*.
- No backwards-compat shims, no premature abstractions, no error handling for impossible cases.
- Every `bonsai grow` step must be idempotent — safe to re-run from CI on every deploy.
- All AWS resources tagged `bonsai:cluster=<name>`, `bonsai:env=<env>`, `bonsai:managed=true`.

## Phase 1 next steps (live)

See the latest Banyan handoff. As of last checkpoint: fill
`internal/provider/aws/{vpc,iam,control_plane,workers}.go` with real SDK
calls in that order, then `internal/cluster/bootstrap.go` for the helm-based
in-cluster installs.
