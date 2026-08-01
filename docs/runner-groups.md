# Runner Group Security

GitHub decides which repositories can route jobs to a self-hosted runner through its runner group before EPAR receives a job. EPAR therefore verifies the configured group policy before creating a registration token; it does not accept a job first and reject it afterward.

## Recommended GitHub Setup

1. Open the organization’s **Settings**, then **Actions**, **Runners**, and **Runner groups**.
2. Create a dedicated group for EPAR runners.
3. Choose **Selected repositories** and add only repositories whose workflows are trusted to run on the EPAR host.
4. Keep public repository access disabled.
5. Run `./start` and select that group when the wizard lists the organization’s live runner groups.

See GitHub’s [runner-group access documentation](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access) for the organization and enterprise controls.

Any repository with access to the group can route a matching job to its runners. Broad access also applies to repositories created later, and public repositories can expose self-hosted runners to untrusted pull request or fork workflows. Provider isolation does not replace this authorization boundary: Docker Sandboxes isolates each runner inside a dedicated microVM, but a job can still access its assigned secrets and reachable services; Docker Container and WSL should remain limited to trusted workflows.

## Wizard Decisions

The wizard requires an explicit numbered selection; pressing Enter does not select the default group. It blocks groups that permit public repositories under the generated safety policy.

GitHub's runner-group API does not provide a creation timestamp, so the wizard cannot order groups by age. It orders them by security posture instead: groups with public repository access disabled appear first, then selected-repository access before all-private access before all-repository access. Within equivalent policies, non-default organization-managed groups appear before default or inherited alternatives. The wizard explains each access mode in plain language and labels groups as recommended, requiring review, not recommended, or blocked.

The default group and groups available to all private or all repositories are not automatically forbidden. The wizard explains their broader and potentially future access, then offers **Continue with this group** or **Back to group selection**. Continuing records that deliberate choice in the policy. Inherited enterprise groups receive an additional warning because their policy must be managed at enterprise level.

The wizard must read the live GitHub policy. It has no offline or unchecked group-name fallback, and it does not write or overwrite a config if the API call or selection fails.

## Configuration

```yaml
runner:
  group: your-runner-group

security:
  runnerGroup:
    enforcement: enforce
    requireExplicitGroup: true
    requireNonDefaultGroup: true
    requiredRepositoryAccess: selected
    requirePublicRepositoriesDisabled: true
```

`enforcement` accepts `enforce` or `warn`. Enforcement blocks new runner registrations when GitHub cannot be checked or the policy violates a requirement. Warning mode reports the same conditions and continues; configs created before this feature use strict recommended requirements in warning mode until migrated.

`requiredRepositoryAccess` is the maximum permitted breadth. `selected` permits only selected-repository access, `private` permits selected or all-private access, and `all` permits any GitHub repository-access setting. Tightening GitHub policy remains valid; broadening beyond the configured ceiling blocks registration.

`requirePublicRepositoriesDisabled` is independent of repository breadth. Setting it to `false` deliberately allows public repositories but produces a runtime advisory. This override should be exceptional and should be paired with a documented trusted-workflow threat model.

Setting `requireNonDefaultGroup` to `false` permits the default group but still produces an advisory when it is used. Setting `requireExplicitGroup` to `false` permits an empty `runner.group`; EPAR then resolves and checks the group GitHub marks as default.

## Runtime Behavior

EPAR checks policy before startup image work, before registration-enabled pool provisioning, and again immediately before each runner registration token request. Enforcement failures do not delete existing runners or interrupt jobs. EPAR does not modify GitHub policy and does not run a periodic policy audit.

An administrator can change GitHub policy between the final check and registration because GitHub does not provide an atomic policy-check-and-register operation. Keeping group administration restricted remains necessary.
