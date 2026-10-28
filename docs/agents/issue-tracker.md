# Issue tracker: GitHub

Issues and specs live in `the-inconvenience-store/nx-remote-cache`.
Use `gh` with explicit `--repo the-inconvenience-store/nx-remote-cache`.

## Operations

- Create: `gh issue create --repo the-inconvenience-store/nx-remote-cache --title "..." --body "..."`
- Read: `gh issue view <number> --repo the-inconvenience-store/nx-remote-cache --json number,title,body,labels,comments`
- List: `gh issue list --repo the-inconvenience-store/nx-remote-cache --state open --json number,title,body,labels,comments`
- Comment: `gh issue comment <number> --repo the-inconvenience-store/nx-remote-cache --body "..."`
- Label: `gh issue edit <number> --repo the-inconvenience-store/nx-remote-cache --add-label "<label>"`; remove with `--remove-label`.
- Close: `gh issue close <number> --repo the-inconvenience-store/nx-remote-cache --comment "..."`

“Publish to the issue tracker” means create an issue. “Fetch the relevant
ticket” means read that issue. Use a heredoc for multiline bodies.

## Labels

Labels must exist before use: GitHub rejects an issue creation carrying
a missing label. List with
`gh label list --repo the-inconvenience-store/nx-remote-cache --limit 100 --json name`.
Get approval before creating missing labels with
`gh label create "<label>" --repo the-inconvenience-store/nx-remote-cache`.

Triage roles are mapped in `triage-labels.md`.
Wayfinder uses `wayfinder:map`, `wayfinder:research`, `wayfinder:prototype`,
`wayfinder:grilling`, and `wayfinder:task`.

## Pull requests as a triage surface

**PRs as a request surface: no.**

GitHub shares issue and PR numbers. When the type is unknown, try
`gh pr view <number> --repo the-inconvenience-store/nx-remote-cache`,
then fall back to the issue read command.

## Wayfinding operations

- Map: one issue labelled `wayfinder:map`, with Notes, Decisions-so-far,
  and Fog in its body.
- Child: label `wayfinder:<type>` and link as a native sub-issue.
  Check `gh --version` and command help before using version-dependent flags.
  API fallback: `gh api --method POST repos/the-inconvenience-store/nx-remote-cache/issues/<parent>/sub_issues -F sub_issue_id=<child-database-id>`.
  If sub-issues are unavailable, use a parent task list and `Part of #<parent>`
  in each child body.
- Blocking: use native issue dependencies. API:
  `gh api --method POST repos/the-inconvenience-store/nx-remote-cache/issues/<child>/dependencies/blocked_by -F issue_id=<blocker-database-id>`.
  Resolve database IDs with
  `gh api repos/the-inconvenience-store/nx-remote-cache/issues/<number> --jq .id`.
  If dependencies are unavailable, record `Blocked by #<number>` in the body.
- Frontier: among the map's open children, exclude assigned tickets and
  tickets with open blockers; choose the first in map order.
- Claim: `gh issue edit <number> --repo the-inconvenience-store/nx-remote-cache --add-assignee @me`.
- Resolve: comment with the answer, close the child, and append its result
  and link to the map's Decisions-so-far.
