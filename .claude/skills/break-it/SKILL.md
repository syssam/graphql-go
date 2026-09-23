---
name: break-it
description: Prove a test actually guards the code it claims to, by deliberately breaking that code, requiring the test to fail, and restoring it with a reverse edit. Use after writing a test for a fix or guard, when a green suite is being read as evidence, or when asked whether a test is real.
argument-hint: "<test name or guard to check>"
---

A test counts only if it fails when the thing it guards is broken. This repository has several
tests that passed against deliberately broken code; see `.claude/rules/verification.md`.

Target: `$ARGUMENTS`

1. **Identify the guard.** Name the exact line or condition the test is supposed to protect.
   If you cannot point at one, say so. That is already the finding.
2. **Record the current diff** of the file, `git diff -- <file>`, so you can tell afterwards
   that your restore left the working tree exactly as it was.
3. **Break it minimally.** Change one condition, delete one call, or return early. Make it the
   smallest edit that makes the guard wrong. Use the Edit tool.
4. **Run the test** with `-race -count=1`, and the package's full tests as well. Expect a
   failure.
   - It fails: good. Quote the failure line.
   - It passes: the test does not guard this. Check first whether the guard is **deliberately
     redundant** (release through `cancelAll` plus the context tree, idle `Stop` plus the
     `closeIfIdle` re-check, `c.stop(id)` versus the pump's deferred cancel). If it is, break
     both halves together and confirm that fails. Otherwise, report the test as not guarding
     the code.
5. **Restore with a reverse Edit.** Never use `git checkout -- <file>`, `git restore` or
   `git stash`: the file may carry uncommitted work, and those commands destroy it (a hook
   blocks them). Re-run `git diff -- <file>` and confirm it matches step 2 exactly.
6. **Re-run the test** and confirm it passes again.

Report: the guard, the break (as a diff), the failing output, and confirmation of the restore.
For an intermittent race, say how many runs you used; the N+1 race needed about 40 runs with
`-race` to show.
