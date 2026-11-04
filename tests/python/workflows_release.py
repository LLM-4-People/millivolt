"""Release, publish and tag policy."""

from __future__ import annotations

import os
import re
import shlex
import subprocess
import tempfile
from pathlib import Path

from .workflows_support import (
    WORKFLOWS,
    logical_command_lines,
    missing_needles,
    named_step,
    steps,
)

PUBLISH_GUARD = (
    "always() && needs.required.result == 'success' && "
    "github.repository == 'LLM-4-People/millivolt' && "
    "((github.event_name == 'push' && "
    "(github.ref == 'refs/heads/main' || startsWith(github.ref, 'refs/tags/v'))) || "
    "(github.event_name == 'pull_request' && github.event.action == 'closed' && "
    "github.event.pull_request.merged == true && github.base_ref == 'main' && "
    "github.event.pull_request.head.repo.full_name == github.repository && "
    "needs.required.outputs.merged_docs_only != 'true'))"
)
PUBLISH_TESTING_GUARD = (
    "github.event_name == 'push' && github.repository == 'LLM-4-People/millivolt' && "
    "github.ref == 'refs/heads/testing'"
)
PUBLISH_GUARD_NEEDLES = (
    "always()",
    "needs.required.result == 'success'",
    "github.event_name == 'push'",
    "github.repository == 'LLM-4-People/millivolt'",
    "github.ref == 'refs/heads/main'",
    "startsWith(github.ref, 'refs/tags/v')",
    "github.event_name == 'pull_request'",
    "github.event.action == 'closed'",
    "github.event.pull_request.merged == true",
    "github.base_ref == 'main'",
    "github.event.pull_request.head.repo.full_name == github.repository",
    "needs.required.outputs.merged_docs_only != 'true'",
)
PUBLISH_TESTING_GUARD_NEEDLES = (
    "github.event_name == 'push'",
    "github.repository == 'LLM-4-People/millivolt'",
    "github.ref == 'refs/heads/testing'",
)


def publish_guard_errors(publish_if: str, publish_testing_if: str) -> list[str]:
    """Reject any publication condition beyond the exact normalized policy.

    Positive needles alone let an extra `||` ref ride along. The exact string
    makes every event, repository and ref term part of the contract, so a new
    disjunct fails even when all required fragments remain present.
    """
    errors = []
    if publish_if != PUBLISH_GUARD:
        errors.append("check.yml: publish guard must be exactly " + PUBLISH_GUARD)
    if publish_testing_if != PUBLISH_TESTING_GUARD:
        errors.append("check.yml: publish-testing guard must be exactly " + PUBLISH_TESTING_GUARD)
    return errors


def publish_concurrency_errors(concurrency) -> list[str]:
    """The release group owns check-then-create atomicity for the version tag.

    A cancelling group (or a missing flag) could start a second publish while
    the first is between its tag guard and its release create, so both could
    pass the check and publish. The release job must serialize without
    cancellation.
    """
    if not isinstance(concurrency, dict):
        return ["check.yml: publish must declare its release concurrency group"]
    errors = []
    if concurrency.get("group") != "release":
        errors.append("check.yml: publish concurrency group must be release")
    if concurrency.get("cancel-in-progress") is not False:
        errors.append("check.yml: publish concurrency must set cancel-in-progress: false")
    if concurrency.get("queue") != "max":
        errors.append("check.yml: publish concurrency must set queue: max, or a pending release is replaced")
    return errors


MAIN_REF_NAMES = {"main", "refs/heads/main"}
# `git` may carry options before the subcommand, including a value-taking
# `-c key=value` or `-C path`. Stop at a command separator so a later `push`
# in another command cannot make this one look like a push.
GIT_PUSH = re.compile(r"\bgit\b[^;|&]*?\bpush\b")
MAIN_BROAD_PUSH_FLAGS = {"--all", "--branches", "--mirror"}


def main_push_errors(text: str) -> list[str]:
    """Reject every non-comment git push whose destination is main.

    Covers bare and fully qualified refs, `<src>:main` and `:main` deletion
    refspecs, `main:` with an empty destination, `--delete main`, broad
    `--all`/`--branches`/`--mirror` pushes, `git -c ... push`, and commands
    split over continuation lines.
    """
    errors = []
    for line in logical_command_lines(text):
        # A line can chain commands. Check every segment so a safe push before
        # `;` or `&&` cannot hide a main push after it.
        line_has_error = False
        for segment in re.split(r"\s*(?:;|&&|\|\||\|)\s*", line):
            match = GIT_PUSH.search(segment)
            if not match:
                continue
            try:
                arguments = shlex.split(segment[match.end():])
            except ValueError:
                arguments = segment[match.end():].split()
            if any(argument in MAIN_BROAD_PUSH_FLAGS for argument in arguments):
                errors.append("check.yml: a git push line can target main: " + line.strip())
                line_has_error = True
                break
            for argument in arguments:
                if argument.startswith("-"):
                    continue
                refspec = argument.lstrip("+")
                if ":" in refspec:
                    source, destination = refspec.split(":", 1)
                    # `:main` deletes main; `main:` pushes main to an empty
                    # destination, which is still a main refspec.
                    if (destination in MAIN_REF_NAMES or
                            (not destination and source in MAIN_REF_NAMES)):
                        errors.append("check.yml: a git push line targets main: " + line.strip())
                        line_has_error = True
                        break
                elif refspec in MAIN_REF_NAMES:
                    errors.append("check.yml: a git push line targets main: " + line.strip())
                    line_has_error = True
                    break
            if line_has_error:
                break
    return errors


def git(repo: Path, *arguments: str) -> None:
    subprocess.run(["git", "-C", str(repo), *arguments], check=True, text=True,
                   capture_output=True)


def step_script(step_name: str, replacements: dict[str, str]) -> str:
    """Return one step's shell body, dedented, with expressions substituted.

    The release steps run on a runner; executing the extracted body against a
    scratch repository is the only check that pins what the generated notes
    actually say, rather than the wording of the code that writes them.
    """
    text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    marker = f"- name: {step_name}"
    if marker not in text:
        raise AssertionError(f"check.yml: no step named {step_name}")
    body = text.split(marker, 1)[1].split("run: |", 1)[1]
    lines = []
    for line in body.splitlines():
        if line.strip() and not line.startswith("          "):
            break
        lines.append(line[10:] if line.startswith("          ") else "")
    script = "\n".join(lines).rstrip() + "\n"
    for expression, value in replacements.items():
        script = script.replace(expression, value)
    return script


def release_notes_errors() -> list[str]:
    """Run the real release-notes step against a scratch repository.

    Pins the behaviour, not the source: a stable release lists the commits
    since the last stable tag even when its own prerelease tag exists, folded
    internal types stay out of Highlights, version-bump subjects never appear,
    a repeated subject is counted instead of dropping a distinct commit, and
    the changelog link starts at the tag the notes actually list.
    """
    errors = []
    notes = step_script("Create the GitHub release", {
        "${{ steps.image.outputs.digest }}": "sha256:deadbeef",
        "${{ github.repository }}": "LLM-4-People/millivolt",
    })
    # Everything after the notes file is the gh release call, which the scratch
    # repository cannot run.
    # The comparison owner is exercised by cmd/release's own tests; the body
    # test only needs the notes text, so the tag query becomes empty here.
    notes = notes.replace("newest=\"$(git tag -l 'v*' | go run -mod=readonly ./cmd/release -newest)\"", 'newest=""')
    notes = notes.split("\nflags=(", 1)[0]
    if "} > release-notes.md" not in notes:
        return ["check.yml: could not isolate the release-notes body from its creation step"]
    with tempfile.TemporaryDirectory() as scratch:
        repo = Path(scratch)

        def commit(subject: str, version: str = "") -> None:
            if version:
                (repo / "VERSION").write_text(version, encoding="utf-8")
            git(repo, "add", "-A")
            git(repo, "commit", "-q", "--allow-empty", "-m", subject)

        git(repo, "init", "-q", ".")
        git(repo, "config", "user.email", "release@example.com")
        git(repo, "config", "user.name", "release")
        commit("feat: first widget", "1.0.0\n")
        git(repo, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
        commit("queue: drain parked requests")
        commit("docs: describe the drain")
        commit("queue: drain parked requests")
        commit("bump version to 1.1.0", "1.1.0\n")
        git(repo, "tag", "-a", "v1.1.0-rc1", "-m", "v1.1.0-rc1")
        commit("fix: honour the reset instant")
        git(repo, "tag", "-a", "v1.1.0", "-m", "v1.1.0")
        result = subprocess.run(["bash", "-c", notes], cwd=repo, text=True,
                                capture_output=True, check=False,
                                env={**os.environ, "RELEASE_VERSION": "1.1.0"})
        if result.returncode != 0:
            return [f"check.yml: the release-notes step failed on a scratch repository: {result.stderr.strip()}"]
        body = (repo / "release-notes.md").read_text(encoding="utf-8")
        highlights, _, other = body.partition("Other changes")
        for needle, where, label in (
            ("fix: honour the reset instant", highlights, "a fix after the prerelease tag"),
            ("queue: drain parked requests (2 commits)", highlights, "a repeated subject counted once"),
            ("## Highlights", highlights, "the Highlights heading"),
        ):
            if needle not in where:
                errors.append(f"check.yml: release notes lost {label}")
        for needle in ("docs: describe the drain", "test:", "bump version to 1.1.0", "bump version to"):
            if needle in highlights:
                errors.append(f"check.yml: release notes promoted {needle!r} to Highlights")
        if "docs: describe the drain" not in other:
            errors.append("check.yml: release notes dropped the folded docs subject")
        if "compare/v1.0.0...v1.1.0" not in body:
            errors.append("check.yml: the changelog link must start at the last stable tag, not the prerelease")
        if "compare/v1.1.0-rc1" in body:
            errors.append("check.yml: a stable release must not compare against its own prerelease")
    return errors


def release_pipeline_errors() -> list[str]:
    """Run the cut and release steps against scratch repositories.

    Pins the three residual contracts: a failure after the tag became public is
    repaired by a re-run (tag kept, release created), a tag whose release
    already exists stays an idempotent skip, and an out-of-order release does
    not move the latest-release pointer.
    """
    errors = []
    cut = step_script("Cut the next version", {"${{ github.ref_type }}": "branch"})
    notes = step_script("Create the GitHub release", {
        "${{ steps.image.outputs.digest }}": "sha256:deadbeef",
        "${{ github.repository }}": "LLM-4-People/millivolt",
    })
    # The comparison itself is pinned by cmd/release's own tests; here the step
    # only has to wire the answer into the release flags.
    notes = notes.replace("newest=\"$(git tag -l 'v*' | go run -mod=readonly ./cmd/release -newest)\"",
                          'newest="$(cat NEWEST)"')

    def shim(repo: Path, release_exists: bool) -> Path:
        bin_dir = repo / "bin"
        bin_dir.mkdir(exist_ok=True)
        calls = repo / "gh-calls"
        script = bin_dir / "gh"
        script.write_text(
            "#!/bin/sh\n"
            f'echo "$@" >> "{calls}"\n'
            'if [ "$1" = "release" ] && [ "$2" = "view" ]; then exit '
            + ("0" if release_exists else "1") + "; fi\n"
            "exit 0\n", encoding="utf-8")
        script.chmod(0o755)
        return calls

    def run(script: str, repo: Path, outputs: Path, version: str = "") -> subprocess.CompletedProcess:
        env = {**os.environ, "PATH": f"{repo / 'bin'}:{os.environ['PATH']}",
               "GITHUB_OUTPUT": str(outputs), "GITHUB_ENV": str(outputs) + ".env",
               "GITHUB_REF_NAME": f"v{version}"}
        env.pop("GITHUB_REF_TYPE", None)
        if version:
            env["RELEASE_VERSION"] = version
        return subprocess.run(["bash", "-c", script], cwd=repo, text=True,
                              capture_output=True, check=False, env=env)

    def repo_with_tag(version: str, tag: bool, release_exists: bool) -> Path:
        repo = Path(tempfile.mkdtemp())
        git(repo, "init", "-q", ".")
        git(repo, "config", "user.email", "release@example.com")
        git(repo, "config", "user.name", "release")
        (repo / "VERSION").write_text(version + "\n", encoding="utf-8")
        git(repo, "add", "-A")
        git(repo, "commit", "-q", "-m", f"bump version to {version}")
        if tag:
            git(repo, "tag", "-a", f"v{version}", "-m", f"v{version}")
        shim(repo, release_exists)
        return repo

    # A failed publish that left a public tag and no release is repaired by a
    # re-run: the tag stays, the version is built again, the release is created.
    repo = repo_with_tag("1.2.3", tag=True, release_exists=False)
    outputs = repo / "out"
    result = run(cut, repo, outputs)
    fields = dict(line.split("=", 1) for line in outputs.read_text().splitlines() if "=" in line)
    if result.returncode != 0:
        errors.append(f"check.yml: a repairable missing release failed the cut step: {result.stderr.strip()}")
    elif fields.get("repair") != "true" or fields.get("skip") != "false":
        errors.append(f"check.yml: a missing release at HEAD must repair, not skip: {fields}")
    elif fields.get("version") != "1.2.3":
        errors.append(f"check.yml: repair must keep the tagged version, got {fields.get('version')!r}")

    # A tag whose release exists stays an idempotent skip.
    repo = repo_with_tag("1.2.3", tag=True, release_exists=True)
    outputs = repo / "out"
    result = run(cut, repo, outputs)
    fields = dict(line.split("=", 1) for line in outputs.read_text().splitlines() if "=" in line)
    if result.returncode != 0 or fields.get("skip") != "true" or fields.get("repair"):
        errors.append(f"check.yml: a tagged, released version must skip: rc={result.returncode} {fields}")

    # A missing release whose tag is no longer at the checkout cannot be
    # repaired from here.
    repo = repo_with_tag("1.2.3", tag=True, release_exists=False)
    (repo / "later.txt").write_text("later\n", encoding="utf-8")
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", "later work")
    outputs = repo / "out"
    result = run(cut, repo, outputs)
    if result.returncode == 0:
        errors.append("check.yml: a missing release for an older tag must fail, not skip")

    # An out-of-order release must not become GitHub's latest release.
    repo = repo_with_tag("1.1.0", tag=True, release_exists=False)
    (repo / "NEWEST").write_text("1.2.0\n", encoding="utf-8")
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", "backport fix")
    git(repo, "tag", "-a", "v1.1.1", "-m", "v1.1.1")
    calls = repo / "gh-calls"
    if calls.exists():
        calls.unlink()
    outputs = repo / "out"
    result = run(notes, repo, outputs, version="1.1.1")
    if result.returncode != 0:
        errors.append(f"check.yml: the release step failed for an older version: {result.stderr.strip()}")
    elif "--latest=false" not in calls.read_text():
        errors.append("check.yml: an older release must not move the latest pointer")

    # The newest release keeps the default pointer.
    repo = repo_with_tag("1.3.0", tag=False, release_exists=False)
    (repo / "NEWEST").write_text("1.3.0\n", encoding="utf-8")
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", "release work")
    git(repo, "tag", "-a", "v1.3.0", "-m", "v1.3.0")
    calls = repo / "gh-calls"
    if calls.exists():
        calls.unlink()
    outputs = repo / "out"
    result = run(notes, repo, outputs, version="1.3.0")
    if result.returncode != 0:
        errors.append(f"check.yml: the release step failed for the newest version: {result.stderr.strip()}")
    elif "--latest=false" in calls.read_text():
        errors.append("check.yml: the newest release must keep the latest pointer")
    return errors


def check_release(checks: dict, errors: list[str]) -> None:
    jobs = checks.get("jobs") or {}
    publish = jobs.get("publish") or {}
    if publish.get("needs") not in ("required", ["required"]):
        errors.append("check.yml: publish must depend on the required gate")
    publish_if = " ".join(str(publish.get("if", "")).split())
    publish_missing = missing_needles(publish_if, PUBLISH_GUARD_NEEDLES)
    for needle in publish_missing:
        errors.append(f"check.yml: publish guard is missing {needle}")
    publish_testing = jobs.get("publish-testing") or {}
    if publish_testing.get("needs") != ["unit", "browser", "container"]:
        errors.append("check.yml: publish-testing must still wait for unit, browser "
                      "and container")
    publish_testing_if = " ".join(str(publish_testing.get("if", "")).split())
    publish_testing_missing = missing_needles(publish_testing_if, PUBLISH_TESTING_GUARD_NEEDLES)
    for needle in publish_testing_missing:
        errors.append(f"check.yml: publish-testing guard is missing {needle}")
    errors.extend(publish_guard_errors(publish_if, publish_testing_if))
    errors.extend(publish_concurrency_errors(publish.get("concurrency")))
    # Mutation proofs: a cancelling group, a renamed group and a missing flag
    # must all fail, so the atomicity owner cannot silently drift.
    for label, concurrency in (
        ("a cancelling release group", {"group": "release", "cancel-in-progress": True}),
        ("a renamed release group", {"group": "publish", "cancel-in-progress": False}),
        ("a missing cancel flag", {"group": "release"}),
        ("a missing concurrency block", None),
    ):
        if not publish_concurrency_errors(concurrency):
            errors.append(f"workflows self-check: {label} must fail")
    # Mutation proofs: dropping any one event, repository or ref condition from
    # either publication guard must be caught, not masked by the others, and an
    # extra `||` ref must fail the exact normalized contract.
    for guard, needles, label in (
        (publish_if, PUBLISH_GUARD_NEEDLES, "publish"),
        (publish_testing_if, PUBLISH_TESTING_GUARD_NEEDLES, "publish-testing"),
    ):
        for needle in needles:
            if not missing_needles(guard.replace(needle, ""), needles):
                errors.append(f"workflows self-check: removing the {label} guard {needle} must fail")
    forbidden_publish = publish_if + " || github.ref == 'refs/heads/testing'"
    if not publish_guard_errors(forbidden_publish, publish_testing_if):
        errors.append("workflows self-check: adding a forbidden ref to the publish guard must fail")
    forbidden_testing = publish_testing_if + " || github.ref == 'refs/heads/main'"
    if not publish_guard_errors(publish_if, forbidden_testing):
        errors.append("workflows self-check: adding a forbidden ref to the publish-testing guard must fail")
    # A main push deliberately skips checks and container. Without always(),
    # GitHub's implicit success gate also skips publish even though required
    # passes, so the release never gets tagged.
    without_always = publish_if.replace("always() && ", "", 1)
    if not publish_guard_errors(without_always, publish_testing_if):
        errors.append("workflows self-check: dropping always() from publish must fail")
    without_required = publish_if.replace("needs.required.result == 'success' && ", "", 1)
    if not publish_guard_errors(without_required, publish_testing_if):
        errors.append("workflows self-check: dropping the required success check must fail")

    publish_checkout = next((step for step in steps(publish)
                             if str(step.get("uses", "")).startswith("actions/checkout@")), {})
    publish_ref = " ".join(str((publish_checkout.get("with") or {}).get("ref", "")).split())
    if publish_ref != "${{ github.event.pull_request.merge_commit_sha || github.sha }}":
        errors.append("check.yml: publish checkout must pin the event commit, never a branch tip")
    verify_target = next((step for step in steps(publish)
                          if step.get("name") == "Verify release target"), {})
    if "if" in verify_target:
        errors.append("check.yml: the release target check must run for every event, not only a merged PR")
    if (verify_target.get("env") or {}).get("EXPECTED") != "${{ github.event.pull_request.merge_commit_sha || github.sha }}":
        errors.append("check.yml: the release target check must compare against the event commit")
    verify_run = str(verify_target.get("run", ""))
    if "git rev-parse HEAD" not in verify_run or '!= "$EXPECTED"' not in verify_run:
        errors.append("check.yml: publish must verify the checked-out commit")

    image_index = named_step(publish, "Publish verified multi-platform image")
    tag_push_index = named_step(publish, "Push verified tag")
    release_index = named_step(publish, "Create the GitHub release")
    if min(image_index, tag_push_index, release_index) < 0:
        errors.append("check.yml: publish image, tag push and release steps must all exist")
    elif not (image_index < tag_push_index < release_index):
        errors.append("check.yml: image must be published before the tag and release")
    image = steps(publish)[image_index] if image_index >= 0 else {}
    if image.get("with", {}).get("sbom") is not True:
        errors.append("check.yml: release image must include an SBOM")
    if image.get("with", {}).get("provenance") != "mode=max":
        errors.append("check.yml: release image must include max provenance")
    attest_index = named_step(publish, "Attest the published image")
    if attest_index < 0:
        errors.append("check.yml: release image must carry a GitHub artifact attestation")
    elif not (image_index < attest_index < tag_push_index):
        errors.append("check.yml: the workflow-pushed Git tag must follow the image attestation")
    else:
        attest = steps(publish)[attest_index]
        if not str(attest.get("uses", "")).startswith("actions/attest@"):
            errors.append("check.yml: the image attestation must use actions/attest")
        attest_with = attest.get("with") or {}
        if attest_with.get("subject-digest") != "${{ steps.image.outputs.digest }}":
            errors.append("check.yml: the attestation must cover the published image digest")
        if attest_with.get("push-to-registry") is not True:
            errors.append("check.yml: the attestation must be pushed to the registry")
    cut = next((step for step in steps(publish) if step.get("name") == "Cut the next version"), {})
    cut_run = str(cut.get("run", ""))
    if "git push" in cut_run:
        errors.append("check.yml: the cut step must not push before the image exists")
    if "git commit" in cut_run:
        errors.append("check.yml: the release workflow must not create version commits")
    if "GH_TOKEN" not in (cut.get("env") or {}):
        errors.append("check.yml: the cut step must read the GitHub release with GH_TOKEN")
    release_guard = (
        'tagged_revision="$(git rev-list -n1 "v$file_version")"',
        'git merge-base --is-ancestor "$revision" origin/main',
        'gh release view "v$file_version"',
        "repairing on this run",
        "The release cannot be repaired from a newer commit",
        "exit 1",
    )
    for needle in missing_needles(cut_run, release_guard):
        errors.append(f"check.yml: cut step is missing the missing-release guard {needle}")
    for needle in release_guard:
        if not missing_needles(cut_run.replace(needle, ""), release_guard):
            errors.append(f"workflows self-check: removing the missing-release guard {needle} must fail")
    tag_push = steps(publish)[tag_push_index] if tag_push_index >= 0 else {}
    tag_push_run = str(tag_push.get("run", ""))
    if '"v$RELEASE_VERSION"' not in tag_push_run:
        errors.append("check.yml: the release workflow must push the verified tag")
    if "skip != 'true'" not in str(tag_push.get("if", "")):
        errors.append("check.yml: the tag push must honor the no-release guard")
    if "repair != 'true'" not in str(tag_push.get("if", "")):
        errors.append("check.yml: a repair run must not re-push the existing tag")
    release_step = steps(publish)[release_index] if release_index >= 0 else {}
    release_run = str(release_step.get("run", ""))
    for needle in (
        "## Highlights",
        "## Compatibility",
        "## Upgrade",
        "docker pull ghcr.io/llm-4-people/millivolt:${RELEASE_VERSION}",
        "<summary>Other changes</summary>",
        "gh attestation verify oci://ghcr.io/llm-4-people/millivolt:${RELEASE_VERSION}",
    ):
        if needle not in release_run:
            errors.append(f"check.yml: release notes are missing {needle}")
    # Highlights are chosen by an internal-type rule, not by a feat/fix/perf
    # prefix: this repository writes behaviour commits as "scope: summary"
    # ("queue: let the operator drain parked requests"), and the prefix rule
    # reported "Maintenance and internal changes only" for the feature release
    # 0.4.10. The real classifier lines run here against a fixture, so the
    # behaviour is pinned rather than the wording of the comment above them.
    classifier = tuple(
        line.strip() for line in release_run.splitlines()
        if line.strip().startswith(("internal_types=", "highlights=", "internal="))
    )
    behaviour, internal_subject = "queue: let the operator drain parked requests", "docs: describe the drain"
    if len(classifier) != 3:
        errors.append("check.yml: release notes must classify subjects in exactly one place")
    else:
        script = "\n".join((
            "set -eu",
            'subjects="$(cat)"',
            *classifier,
            'printf "HIGHLIGHTS\\n%s\\nOTHER\\n%s\\n" "$highlights" "$internal"',
        ))
        fixture = "\n".join((behaviour, internal_subject, "deps(go): bump golang.org/x/net",
                             "test: pin the classifier", ""))
        result = subprocess.run(["bash", "-c", script], input=fixture,
                                text=True, capture_output=True, check=False)
        if result.returncode != 0:
            errors.append(f"check.yml: the release classifier failed on a subject fixture: {result.stderr.strip()}")
        else:
            highlights, _, other = result.stdout.partition("OTHER\n")
            if behaviour not in highlights:
                errors.append("check.yml: a scope-only behaviour commit is not a highlight")
            if internal_subject in highlights:
                errors.append("check.yml: a docs commit was promoted to Highlights")
            for subject in (internal_subject, "deps(go): bump golang.org/x/net", "test: pin the classifier"):
                if subject not in other:
                    errors.append(f"check.yml: {subject!r} must stay under Other changes")
    # The promotion VERSION gate and the latest-release decision share one
    # comparison owner. A stray tag that is not a release version must be
    # ignored (cmd/release -newest reports it), never fed to -newer-than, and
    # the loop over every tag must not come back.
    promotion = next((step for step in steps(jobs.get("required") or {})
                      if step.get("name") == "Require an unreleased version for main promotions"), {})
    promotion_run = str(promotion.get("run", ""))
    if "-newest" not in promotion_run or 'git tag -l \'v*\'' not in promotion_run:
        errors.append("check.yml: the promotion VERSION gate must compare against the newest release tag")
    if 'while IFS= read -r tag' in promotion_run or '"${tag#v}"' in promotion_run:
        errors.append("check.yml: the promotion VERSION gate must not compare raw tag names")
    if 'newest="$(git tag -l \'v*\' | go run -mod=readonly ./cmd/release -newest)"' not in release_run:
        errors.append("check.yml: the release step must ask cmd/release for the newest version")
    if '--latest=false' not in release_run:
        errors.append("check.yml: an older release must not move the latest pointer")
    summary_step = next((step for step in steps(publish)
                         if step.get("name") == "Summarize the release"), {})
    summary_run = str(summary_step.get("run", ""))
    if not summary_step:
        errors.append("check.yml: publish must summarize the release in the run summary")
    else:
        if "always()" not in str(summary_step.get("if", "")):
            errors.append("check.yml: the release summary must run for both success and skip")
        for needle in ("No release cut", "releases/tag/v${RELEASE_VERSION}", "GITHUB_STEP_SUMMARY",
                       "job.status", "no release receipt is claimed"):
            if needle not in summary_run:
                errors.append(f"check.yml: release summary is missing {needle}")
        if "VERSION is already tagged at HEAD" in summary_run:
            errors.append("check.yml: the skip summary must not claim the tag is at HEAD")
    meta = next((step for step in steps(publish)
                 if step.get("name") == "Define image tags and labels"), {})
    meta_with = meta.get("with") or {}
    if "org.opencontainers.image.description" not in str(meta_with.get("annotations", "")):
        errors.append("check.yml: release image must carry a package description annotation")
    if (meta.get("env") or {}).get("DOCKER_METADATA_ANNOTATIONS_LEVELS") != "index,manifest":
        errors.append("check.yml: package annotations must target the index and manifest")
    publish_text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    errors.extend(main_push_errors(publish_text))
    for label, line in (
        ("a bare main refspec", "git push origin main"),
        ("a fully qualified main refspec", "git push origin refs/heads/main"),
        ("a main refspec destination", "git push origin HEAD:refs/heads/main"),
        ("a same-name main destination", "git push origin main:main"),
        ("a short main destination", "git push origin HEAD:main"),
        ("a main deletion refspec", "git push origin :main"),
        ("a qualified main deletion refspec", "git push origin :refs/heads/main"),
        ("an empty main destination", "git push origin main:"),
        ("an empty qualified main destination", "git push origin refs/heads/main:"),
        ("a git -c main push", "git -c user.name=release push origin main"),
        ("a git -c main destination", "git -c advice.detachedHead=false push origin HEAD:main"),
        ("a git option before push", "git --no-pager push origin main"),
        ("a --delete main branch", "git push --delete origin main"),
        ("a broad --all push", "git push --all origin"),
        ("a continued main push", "git push \\\n  origin main"),
        ("a quoted main destination", "git push origin HEAD:'refs/heads/main'"),
        ("a main push chained after a safe push",
         "git push origin HEAD:testing; git push origin main"),
        ("a main push chained with &&",
         "git push origin HEAD:testing && git -c user.name=release push origin main"),
        ("a continued git -c main push",
         "git -c \\\n  user.name=release push \\\n  origin main"),
    ):
        if not main_push_errors(line):
            errors.append(f"workflows self-check: {label} must fail")
    for label, line in (
        ("a testing refspec push", "git push origin HEAD:testing"),
        ("a source-main testing destination", "git push origin main:testing"),
        ("a qualified testing destination", "git push origin HEAD:refs/heads/testing"),
        ("a git -c testing push", "git -c user.name=release push origin HEAD:testing"),
        ("a --delete feature branch", "git push --delete origin feature"),
        ("a tags-only push", "git push --tags origin"),
        ("a continued testing push", "git push \\\n  origin HEAD:testing"),
    ):
        if main_push_errors(line):
            errors.append(f"workflows self-check: {label} must pass")
    if "ghcr.io/${REPO}" in publish_text or "ghcr.io/LLM-4-People" in publish_text:
        errors.append("check.yml: release image references must use lowercase GHCR paths")
    if "value=main,enable=${{ github.ref == 'refs/heads/main' }}" in publish_text:
        errors.append("check.yml: main image must exclude prereleases")
    errors.extend(release_notes_errors())
    errors.extend(release_pipeline_errors())
